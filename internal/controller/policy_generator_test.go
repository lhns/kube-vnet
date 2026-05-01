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

// Test-only helper: build a MembersByNS for the common case of all-bidirectional,
// bare-form-in-home-prefixed-elsewhere members. Mirrors what discoverMembers
// would produce.
func bidiMembers(homeNS string, byNS map[string][]string) map[string]map[KeyForm]map[Direction][]string {
	out := map[string]map[KeyForm]map[Direction][]string{}
	for ns, pods := range byNS {
		form := KeyPrefixed
		if ns == homeNS {
			form = KeyBare
		}
		out[ns] = map[KeyForm]map[Direction][]string{
			form: {DirectionBoth: pods},
		}
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
		MembersByNS: bidiMembers("platform", map[string][]string{"platform": {"orders-1", "orders-2"}}),
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	if p.Namespace != "platform" || p.Name != "kube-vnet-payments-platform" {
		t.Errorf("unexpected name/ns: %s/%s", p.Namespace, p.Name)
	}
	if p.Labels[LabelManagedBy] != LabelManagedByValue || p.Labels[LabelNetwork] != "platform.payments" {
		t.Errorf("unexpected labels: %v", p.Labels)
	}
	if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].Name != "payments" {
		t.Errorf("expected owner ref to payments")
	}
	if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.payments" {
		t.Errorf("podSelector key=%s want kube-vnet/net.payments", got)
	}
	if op := p.Spec.PodSelector.MatchExpressions[0].Operator; op != metav1.LabelSelectorOpIn {
		t.Errorf("podSelector operator=%v want In", op)
	}
	wantValues := []string{"true", "both"}
	if got := p.Spec.PodSelector.MatchExpressions[0].Values; !equalStringSlice(got, wantValues) {
		t.Errorf("podSelector values=%v want %v", got, wantValues)
	}
	// Membership policies are ingress-only (ADR 0025): no egress restriction.
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
		MembersByNS: bidiMembers("monitoring", map[string][]string{
			"platform": {"a"},
			"webapp":   {"b"},
		}),
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(out.Policies))
	}
	for _, p := range out.Policies {
		want := "kube-vnet/net.monitoring.observability"
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != want {
			t.Errorf("ns=%s podSelector key=%s want %s", p.Namespace, got, want)
		}
		if len(p.OwnerReferences) != 0 {
			t.Errorf("ns=%s should not have owner ref (cross-namespace)", p.Namespace)
		}
	}
}

func TestGenerate_HomeAndForeignMixed(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "monitoring"),
		MembersByNS: bidiMembers("monitoring", map[string][]string{
			"monitoring": {"home-pod"},
			"platform":   {"a"},
		}),
	})
	var sawHome, sawForeign bool
	for i := range out.Policies {
		p := &out.Policies[i]
		switch p.Namespace {
		case "monitoring":
			if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.observability" {
				t.Errorf("home selector key=%s want bare form", got)
			}
			if len(p.OwnerReferences) != 1 {
				t.Errorf("home policy must own-ref the vnet")
			}
			sawHome = true
		case "platform":
			if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.monitoring.observability" {
				t.Errorf("foreign selector key=%s want prefixed form", got)
			}
			sawForeign = true
		}
	}
	if !sawHome || !sawForeign {
		t.Fatalf("expected both home and foreign policies")
	}
}

// TestGenerate_DirectionEnum_OneOfEach: a single namespace with three pods,
// one of each direction, generates two self-policies (bidi, ingress) — the
// `egress`-only pod produces no self-policy because it accepts no ingress and
// the operator no longer restricts egress (ADR 0025).
func TestGenerate_DirectionEnum_OneOfEach(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[KeyForm]map[Direction][]string{
			"platform": {
				KeyBare: {
					DirectionBoth:    {"a"},
					DirectionIngress: {"b"},
					DirectionEgress:  {"c"},
				},
			},
		},
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies (bidi + ingress; no policy for egress-only), got %d", len(out.Policies))
	}

	byName := map[string]*kpolicySummary{}
	for i := range out.Policies {
		p := &out.Policies[i]
		byName[p.Name] = summarize(p)
	}

	bidi, ok := byName["kube-vnet-payments-platform"]
	if !ok {
		t.Fatalf("missing bidi policy")
	}
	if !equalStringSlice(bidi.podSelectorValues, []string{"true", "both"}) {
		t.Errorf("bidi podSelector values=%v", bidi.podSelectorValues)
	}
	if !bidi.hasIngress || bidi.hasEgress {
		t.Errorf("bidi should have Ingress only (egress is never restricted)")
	}

	ingress, ok := byName["kube-vnet-payments-platform-ingress"]
	if !ok {
		t.Fatalf("missing ingress-only policy")
	}
	if !equalStringSlice(ingress.podSelectorValues, []string{"ingress"}) {
		t.Errorf("ingress podSelector values=%v", ingress.podSelectorValues)
	}
	if !ingress.hasIngress || ingress.hasEgress {
		t.Errorf("ingress-only should have Ingress only")
	}

	if _, exists := byName["kube-vnet-payments-platform-egress"]; exists {
		t.Errorf("egress-only direction must NOT produce a self-policy")
	}
}

// TestGenerate_DirectionEnum_PeerSelectorsNarrowed: peer rules narrow to
// initiator-capable peers via `In [true, both, egress]` selectors.
func TestGenerate_DirectionEnum_PeerSelectorsNarrowed(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[KeyForm]map[Direction][]string{
			"platform": {KeyBare: {DirectionBoth: {"a"}}},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	from := p.Spec.Ingress[0].From[0].PodSelector.MatchExpressions[0]
	if !equalStringSlice(from.Values, []string{"true", "both", "egress"}) {
		t.Errorf("ingress.from peer values=%v want [true both egress]", from.Values)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("expected no egress section, got %d rules", len(p.Spec.Egress))
	}
}

// TestGenerate_LongFormInHome: pods in the home namespace with both bare AND
// prefixed forms generate two policies in the home namespace (one per form).
func TestGenerate_LongFormInHome(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[KeyForm]map[Direction][]string{
			"platform": {
				KeyBare:     {DirectionBoth: {"orders"}},
				KeyPrefixed: {DirectionBoth: {"reports"}},
			},
		},
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies (bare + prefixed), got %d", len(out.Policies))
	}
	var bare, pref *summaryByName
	for i := range out.Policies {
		p := &out.Policies[i]
		s := &summaryByName{name: p.Name, key: p.Spec.PodSelector.MatchExpressions[0].Key}
		if strings.HasSuffix(p.Name, "-prefixed") {
			pref = s
		} else {
			bare = s
		}
	}
	if bare == nil || bare.key != "kube-vnet/net.payments" {
		t.Errorf("bare policy missing or wrong key: %+v", bare)
	}
	if pref == nil || pref.key != "kube-vnet/net.platform.payments" {
		t.Errorf("prefixed policy missing or wrong key: %+v", pref)
	}
}

// TestGenerate_LongFormInHome_OnlyPrefixed: pods in the home namespace using
// only the prefixed form generate ONE policy with the -prefixed suffix.
func TestGenerate_LongFormInHome_OnlyPrefixed(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[KeyForm]map[Direction][]string{
			"platform": {KeyPrefixed: {DirectionBoth: {"reports"}}},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	if p.Name != "kube-vnet-payments-platform-prefixed" {
		t.Errorf("name=%s want kube-vnet-payments-platform-prefixed", p.Name)
	}
	if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.platform.payments" {
		t.Errorf("podSelector key=%s", got)
	}
}

func TestGenerate_Binding_EmitsPerBindingPolicy(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		BindingsByNS: map[string][]BindingSpec{
			"webapp": {{
				Name:        "thirdparty",
				Direction:   DirectionBoth,
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "vendor-x"}},
			}},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	if p.Namespace != "webapp" || p.Name != "kube-vnet-payments-b-thirdparty" {
		t.Errorf("policy ns/name = %s/%s", p.Namespace, p.Name)
	}
	if v := p.Labels[LabelBinding]; v != "thirdparty" {
		t.Errorf("LabelBinding=%q", v)
	}
	if got := p.Spec.PodSelector.MatchLabels["app"]; got != "vendor-x" {
		t.Errorf("podSelector did not match binding selector verbatim: %v", p.Spec.PodSelector)
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("PolicyTypes=%v want [Ingress] only", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("expected no egress section on a binding policy, got %d", len(p.Spec.Egress))
	}
}

func TestGenerate_Binding_AddsPeerEntryToLabelDrivenPolicy(t *testing.T) {
	// Label-driven member in the home namespace + a binding in webapp.
	// The label-driven policy's peer rules must include both a label
	// selector and the binding's verbatim selector.
	out := Generate(GenerateInput{
		VNet:        newVNet("payments", "platform"),
		MembersByNS: bidiMembers("platform", map[string][]string{"platform": {"a-1"}}),
		BindingsByNS: map[string][]BindingSpec{
			"webapp": {{
				Name:        "thirdparty",
				Direction:   DirectionBoth,
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "vendor-x"}},
			}},
		},
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies (label + binding), got %d", len(out.Policies))
	}
	var labelPolicy *networkingv1.NetworkPolicy
	for i := range out.Policies {
		if out.Policies[i].Name == "kube-vnet-payments-platform" {
			labelPolicy = &out.Policies[i]
		}
	}
	if labelPolicy == nil {
		t.Fatalf("did not find the label-driven policy")
	}
	if len(labelPolicy.Spec.Ingress) != 1 || len(labelPolicy.Spec.Ingress[0].From) != 2 {
		t.Fatalf("expected 2 ingress.from entries (label + binding), got %#v", labelPolicy.Spec.Ingress)
	}
	// One of the entries should match the binding's verbatim selector.
	found := false
	for _, peer := range labelPolicy.Spec.Ingress[0].From {
		if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "vendor-x" {
			found = true
		}
	}
	if !found {
		t.Errorf("no peer entry referenced the binding's selector: %#v", labelPolicy.Spec.Ingress[0].From)
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
		{"true", DirectionBoth, true},
		{"", DirectionBoth, true}, // legacy: presence-only meant member
		{"ingress", DirectionIngress, true},
		{"egress", DirectionEgress, true},
		{"false", DirectionNone, true},
		{"none", DirectionNone, true},
		{"yes", DirectionNone, false},
		{"INGRESS", DirectionNone, false}, // case-sensitive
		{"both ", DirectionNone, false},   // no whitespace stripping
	}
	for _, c := range cases {
		got, ok := ParseDirection(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseDirection(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
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
