package controller

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func newVNet(name, ns string) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
	}
}

// Test-only helper: build a MembersByNS for the common case of all-
// bidirectional members. Per ADR 0033 there's no key-form axis — every
// member is selected via the canonical FQ system label.
func bidiMembers(byNS map[string][]string) map[string]map[Direction][]string {
	out := map[string]map[Direction][]string{}
	for ns, pods := range byNS {
		out[ns] = map[Direction][]string{DirectionBoth: pods}
	}
	return out
}

func TestGenerate_NoMembers(t *testing.T) {
	out := Generate(GenerateInput{
		VNet:        newVNet("payments", "platform"),
		MembersByNS: nil,
	})
	if len(out.Policies) != 0 {
		t.Fatalf("expected 0 policies, got %d", len(out.Policies))
	}
}

func TestGenerate_HomeNamespaceOnly(t *testing.T) {
	out := Generate(GenerateInput{
		VNet:        newVNet("payments", "platform"),
		MembersByNS: bidiMembers(map[string][]string{"platform": {"orders-1", "orders-2"}}),
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	wantName := PolicyName("payments", "platform")
	if p.Namespace != "platform" || p.Name != wantName {
		t.Errorf("unexpected name/ns: %s/%s want platform/%s", p.Namespace, p.Name, wantName)
	}
	if p.Labels[LabelManagedBy] != LabelManagedByValue || p.Labels[LabelNetwork] != "platform.payments" {
		t.Errorf("unexpected labels: %v", p.Labels)
	}
	if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].Name != "payments" {
		t.Errorf("expected owner ref to payments")
	}
	// Per ADR 0033, the canonical FQ system label is the only selector key.
	if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet.system/net.platform.payments" {
		t.Errorf("podSelector key=%s want kube-vnet.system/net.platform.payments", got)
	}
	if op := p.Spec.PodSelector.MatchExpressions[0].Operator; op != metav1.LabelSelectorOpIn {
		t.Errorf("podSelector operator=%v want In", op)
	}
	wantValues := []string{"both", "ingress"}
	if got := p.Spec.PodSelector.MatchExpressions[0].Values; !equalStringSlice(got, wantValues) {
		t.Errorf("podSelector values=%v want %v", got, wantValues)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Egress) != 0 {
		t.Errorf("ingress=%d egress=%d (want 1 ingress, 0 egress)", len(p.Spec.Ingress), len(p.Spec.Egress))
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes=%v want [Ingress]", p.Spec.PolicyTypes)
	}
}

func TestGenerate_TwoNamespaces(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "monitoring"),
		MembersByNS: bidiMembers(map[string][]string{
			"platform": {"a"},
			"webapp":   {"b"},
		}),
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(out.Policies))
	}
	for _, p := range out.Policies {
		want := "kube-vnet.system/net.monitoring.observability"
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != want {
			t.Errorf("ns=%s podSelector key=%s want %s", p.Namespace, got, want)
		}
		if len(p.OwnerReferences) != 0 {
			t.Errorf("ns=%s should not have owner ref (cross-namespace)", p.Namespace)
		}
	}
}

// TestGenerate_HomeAndForeignSameSelector: home and foreign-NS policies
// have the same canonical FQ selector key (per ADR 0033, no bare/prefixed
// split on the operator output).
func TestGenerate_HomeAndForeignSameSelector(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "monitoring"),
		MembersByNS: bidiMembers(map[string][]string{
			"monitoring": {"home-pod"},
			"platform":   {"a"},
		}),
	})
	wantKey := "kube-vnet.system/net.monitoring.observability"
	wantName := PolicyName("observability", "monitoring")
	var sawHome, sawForeign bool
	for i := range out.Policies {
		p := &out.Policies[i]
		if p.Name != wantName {
			t.Errorf("policy name=%q want %q (uniform per ADR 0033)", p.Name, wantName)
		}
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != wantKey {
			t.Errorf("ns=%s podSelector key=%s want %s", p.Namespace, got, wantKey)
		}
		switch p.Namespace {
		case "monitoring":
			if len(p.OwnerReferences) != 1 {
				t.Errorf("home policy must own-ref the vnet")
			}
			sawHome = true
		case "platform":
			sawForeign = true
		}
	}
	if !sawHome || !sawForeign {
		t.Fatalf("expected both home and foreign policies")
	}
}

// TestGenerate_DirectionEnum_OneOfEach: a single namespace with three pods,
// one of each direction, generates ONE merged self-policy that selects all
// receiver-capable members (`both`, `ingress`). Egress-only pods produce
// no self-policy (no ingress to restrict; egress is unrestricted per ADR 0025).
func TestGenerate_DirectionEnum_OneOfEach(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[Direction][]string{
			"platform": {
				DirectionBoth:    {"a"},
				DirectionIngress: {"b"},
				DirectionEgress:  {"c"},
			},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 merged self-policy, got %d", len(out.Policies))
	}

	p := summarize(&out.Policies[0])
	wantName := PolicyName("payments", "platform")
	if out.Policies[0].Name != wantName {
		t.Errorf("policy name=%q want %q", out.Policies[0].Name, wantName)
	}
	if !equalStringSlice(p.podSelectorValues, []string{"both", "ingress"}) {
		t.Errorf("podSelector values=%v want [both ingress]", p.podSelectorValues)
	}
	if !p.hasIngress || p.hasEgress {
		t.Errorf("merged self-policy should have Ingress only (egress never restricted)")
	}
}

// TestGenerate_DirectionEnum_PeerSelectorsNarrowed: peer rules narrow to
// initiator-capable peers via `In [both, egress]` selectors.
func TestGenerate_DirectionEnum_PeerSelectorsNarrowed(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[Direction][]string{
			"platform": {DirectionBoth: {"a"}},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	from := p.Spec.Ingress[0].From[0].PodSelector.MatchExpressions[0]
	if !equalStringSlice(from.Values, []string{"both", "egress"}) {
		t.Errorf("ingress.from peer values=%v want [both egress]", from.Values)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("expected no egress section, got %d rules", len(p.Spec.Egress))
	}
}

func TestPolicyName_TruncatesWithHash(t *testing.T) {
	long := strings.Repeat("x", 250)
	got := PolicyName(long, long)
	if len(got) > 253 {
		t.Errorf("len=%d exceeds 253", len(got))
	}
	if got != PolicyName(long, long) {
		t.Errorf("not deterministic")
	}
}

// TestPolicyNames_NoCollisions: identity hashing must produce distinct
// names for input pairs that share a textual prefix.
func TestPolicyNames_NoCollisions(t *testing.T) {
	cases := []struct {
		desc string
		a, b string
	}{
		{
			desc: "baseline vs membership where vnet=default ns=deny",
			a:    BaselinePolicyName,
			b:    PolicyName("default", "deny"),
		},
		{
			desc: "vnet=foo ns=bar-baz vs vnet=foo-bar ns=baz",
			a:    PolicyName("foo", "bar-baz"),
			b:    PolicyName("foo-bar", "baz"),
		},
		{
			desc: "shared dot-prefix: <homeNS>.<vnet> vs <homeNS-vnet> (one component)",
			a:    PolicyName("vnet", "home"),
			b:    PolicyName("home-vnet", "default"),
		},
	}
	for _, c := range cases {
		if c.a == c.b {
			t.Errorf("%s: collision on %q", c.desc, c.a)
		}
	}
}

func TestPolicyNames_StableAcrossCalls(t *testing.T) {
	// Identity hashing must be deterministic — same inputs, same name.
	if PolicyName("v", "n") != PolicyName("v", "n") {
		t.Error("PolicyName not deterministic")
	}
}

func TestPolicyNames_Shape(t *testing.T) {
	// Per ADR 0039, every operator-emitted policy carries an explicit kind
	// segment as the second dot-component: base / mem / ext.
	if got := BaselinePolicyName; got != "kube-vnet.base" {
		t.Errorf("baseline = %q want kube-vnet.base", got)
	}
	got := PolicyName("payments", "platform")
	if !strings.HasPrefix(got, "kube-vnet.mem.platform.payments-") {
		t.Errorf("policy shape: %q", got)
	}
}

func TestPolicyNames_KindPrefix(t *testing.T) {
	// Lock in the kind-prefix convention per ADR 0039.
	cases := []struct {
		name    string
		got     string
		want    string
		isExact bool // true → check equality; false → check prefix
	}{
		{"baseline", BaselinePolicyName, "kube-vnet.base", true},
		{"membership_namespaced", PolicyName("foo", "bar"), "kube-vnet.mem.bar.foo-", false},
		{"membership_cluster", PolicyName(SystemVnetCluster, ""), "kube-vnet.mem.cluster-", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.isExact {
				if c.got != c.want {
					t.Errorf("got %q, want %q", c.got, c.want)
				}
			} else {
				if !strings.HasPrefix(c.got, c.want) {
					t.Errorf("got %q, want prefix %q", c.got, c.want)
				}
			}
		})
	}
}

func TestJoinLabelKey(t *testing.T) {
	if got := JoinLabelKey("kube-vnet/", "platform", "payments", "platform"); got != "kube-vnet/net.payments" {
		t.Errorf("same-ns: %s", got)
	}
	if got := JoinLabelKey("kube-vnet/", "platform", "payments", "webapp"); got != "kube-vnet/net.platform.payments" {
		t.Errorf("foreign: %s", got)
	}
}

func TestParseDirection(t *testing.T) {
	type tc struct {
		in   string
		want Direction
		ok   bool
	}
	cases := []tc{
		{"both", DirectionBoth, true},
		{"ingress", DirectionIngress, true},
		{"egress", DirectionEgress, true},
		{"none", DirectionNone, true},
		// default-* variants are valid at baseline tiers (ADR 0031).
		{"default-both", DirectionDefaultBoth, true},
		{"default-ingress", DirectionDefaultIngress, true},
		{"default-egress", DirectionDefaultEgress, true},
		{"default-none", DirectionDefaultNone, true},
		// Legacy aliases dropped per ADR 0030; these are now invalid.
		{"true", DirectionNone, false},
		{"false", DirectionNone, false},
		{"", DirectionNone, false},
		{"yes", DirectionNone, false},
		{"INGRESS", DirectionNone, false}, // case-sensitive
		{"both ", DirectionNone, false},   // no whitespace stripping
		{"default-", DirectionNone, false},
		{"DEFAULT-BOTH", DirectionNone, false},
	}
	for _, c := range cases {
		got, ok := ParseDirection(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseDirection(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseBareDirection_RejectsDefaultPrefix(t *testing.T) {
	for _, v := range []string{"default-both", "default-ingress", "default-egress", "default-none"} {
		if _, ok := ParseBareDirection(v); ok {
			t.Errorf("ParseBareDirection(%q) should reject default-* values (pod tier is bare-only per ADR 0031)", v)
		}
	}
	for _, v := range []string{"both", "ingress", "egress", "none"} {
		if _, ok := ParseBareDirection(v); !ok {
			t.Errorf("ParseBareDirection(%q) rejected a valid bare value", v)
		}
	}
}

func TestDirection_IsDefault(t *testing.T) {
	defaults := []Direction{DirectionDefaultBoth, DirectionDefaultIngress, DirectionDefaultEgress, DirectionDefaultNone}
	for _, d := range defaults {
		if !d.IsDefault() {
			t.Errorf("%s.IsDefault() = false, want true", d)
		}
	}
	bares := []Direction{DirectionBoth, DirectionIngress, DirectionEgress, DirectionNone}
	for _, d := range bares {
		if d.IsDefault() {
			t.Errorf("%s.IsDefault() = true, want false", d)
		}
	}
}

func TestDirection_Bare(t *testing.T) {
	cases := map[Direction]Direction{
		DirectionBoth:           DirectionBoth,
		DirectionIngress:        DirectionIngress,
		DirectionEgress:         DirectionEgress,
		DirectionNone:           DirectionNone,
		DirectionDefaultBoth:    DirectionBoth,
		DirectionDefaultIngress: DirectionIngress,
		DirectionDefaultEgress:  DirectionEgress,
		DirectionDefaultNone:    DirectionNone,
	}
	for in, want := range cases {
		if got := in.Bare(); got != want {
			t.Errorf("%s.Bare() = %s, want %s", in, got, want)
		}
		if got := in.Bare(); got.IsDefault() {
			t.Errorf("%s.Bare() returned a default-prefixed value: %s", in, got)
		}
	}
}

func TestNameRegex(t *testing.T) {
	cases := map[string]bool{
		"payments":     true,
		"a":            true,
		"a-b-c":        true,
		"payments.v2":  false,
		"Payments":     false,
		"-leading":     false,
		"trailing-":    false,
		"":             false,
		"under_score":  false,
	}
	for in, want := range cases {
		got := nameRegex.MatchString(in)
		if got != want {
			t.Errorf("name=%q got=%v want=%v", in, got, want)
		}
	}
}

// --- helpers --------------------------------------------------------------

type kpolicySummary struct {
	podSelectorValues []string
	hasIngress        bool
	hasEgress         bool
}

func summarize(p *networkingv1.NetworkPolicy) *kpolicySummary {
	s := &kpolicySummary{}
	if len(p.Spec.PodSelector.MatchExpressions) > 0 {
		s.podSelectorValues = p.Spec.PodSelector.MatchExpressions[0].Values
	}
	for _, t := range p.Spec.PolicyTypes {
		if t == networkingv1.PolicyTypeIngress {
			s.hasIngress = true
		}
		if t == networkingv1.PolicyTypeEgress {
			s.hasEgress = true
		}
	}
	return s
}

// summaryByName is a small struct used in test reductions.
type summaryByName struct {
	name string
	key  string
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGenerate_ClusterVnet_BareSelectorAndName covers the ADR 0033 Amendment
// end-to-end at the generator level: the cluster vnet's emitted policy uses
// bare `kube-vnet.system/net.cluster` as both the podSelector key and the
// peer from podSelector key, and the policy name is bare `kube-vnet.cluster`.
// Regression test for the bug where SystemLabelKey was missed and the
// cluster vnet silently emitted no working policy.
func TestGenerate_ClusterVnet_BareSelectorAndName(t *testing.T) {
	out := Generate(GenerateInput{
		VNet:        newVNet(SystemVnetCluster, "kube-vnet-system"),
		MembersByNS: bidiMembers(map[string][]string{"traefik": {"traefik-1"}, "webapp": {"server-1"}}),
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies (one per member NS), got %d", len(out.Policies))
	}
	wantName := "kube-vnet." + PolicyKindMembership + "." + SystemVnetCluster
	wantKey := "kube-vnet.system/net." + SystemVnetCluster
	for _, p := range out.Policies {
		if !strings.HasPrefix(p.Name, wantName+"-") {
			t.Errorf("policy name %q does not start with %q", p.Name, wantName+"-")
		}
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != wantKey {
			t.Errorf("ns=%s podSelector key=%q want %q", p.Namespace, got, wantKey)
		}
		for i, peer := range p.Spec.Ingress[0].From {
			if got := peer.PodSelector.MatchExpressions[0].Key; got != wantKey {
				t.Errorf("ns=%s ingress[0].from[%d].podSelector key=%q want %q",
					p.Namespace, i, got, wantKey)
			}
		}
	}
}

// TestSystemLabelKey_ClusterCollapses covers the ADR 0033 Amendment:
// cluster collapses to bare, every other vnet keeps the FQ form.
func TestSystemLabelKey_ClusterCollapses(t *testing.T) {
	cases := []struct {
		homeNS, vnet, want string
	}{
		{"kube-vnet-system", "cluster", "kube-vnet.system/net.cluster"},   // bare
		{"traefik", "namespace", "kube-vnet.system/net.traefik.namespace"}, // per-NS
		{"platform", "payments", "kube-vnet.system/net.platform.payments"}, // user vnet FQ
	}
	for _, tc := range cases {
		if got := SystemLabelKey(tc.homeNS, tc.vnet); got != tc.want {
			t.Errorf("SystemLabelKey(%q, %q) = %q, want %q", tc.homeNS, tc.vnet, got, tc.want)
		}
	}
}
