package controller

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// These tests pin the contract from ADR 0043: `virtualNetworkRef.namespace`
// is optional (inferred when omitted) and HONORED when set — never silently
// rewritten. No vnet kind is special-cased: a wrong namespace simply names a
// vnet the pod cannot join, and is denied by the ordinary permission path.

func ref(name, namespace string) vnetv1alpha1.VirtualNetworkRef {
	return vnetv1alpha1.VirtualNetworkRef{Name: name, Namespace: namespace}
}

// REGRESSION LOCK. The original bug: canonicalVnetKey discarded ref.Namespace
// for the reserved name `namespace` and substituted the pod's own namespace,
// so `{name: namespace, namespace: kube-vnet-system}` silently "worked" —
// resolving to the pod's local namespace vnet even though kube-vnet-system
// has no `namespace` vnet at all. The ref must be honored verbatim.
func TestCanonicalVnetKey_NamespaceVnet_ForeignNamespace_IsNotRewritten(t *testing.T) {
	r := &ResolutionReconciler{}
	got := r.canonicalVnetKey(ref(SystemVnetNamespace, "kube-vnet-system"), "app")
	if want := VnetKey("kube-vnet-system." + SystemVnetNamespace); got != want {
		t.Fatalf("ref.Namespace was rewritten: got %q, want %q "+
			"(the pod's NS must NOT be substituted for an explicit namespace)", got, want)
	}
}

// The uniformity invariant the design rests on: a `namespace` system-vnet ref
// and a user-vnet ref with identical namespace inputs must produce
// structurally identical keys. Guards against re-adding a
// `case SystemVnetNamespace:` branch.
func TestCanonicalVnetKey_NamespaceVnet_BehavesLikeUserVnet(t *testing.T) {
	r := &ResolutionReconciler{}
	const podNS = "app"
	for _, tc := range []struct {
		name    string
		refNS   string
		wantFmt string // "<ns>.<name>"
	}{
		{"omitted infers local", "", podNS},
		{"explicit local", podNS, podNS},
		{"explicit foreign", "other", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sys := r.canonicalVnetKey(ref(SystemVnetNamespace, tc.refNS), podNS)
			usr := r.canonicalVnetKey(ref("web", tc.refNS), podNS)
			if wantSys := VnetKey(tc.wantFmt + "." + SystemVnetNamespace); sys != wantSys {
				t.Errorf("system `namespace` vnet: got %q, want %q", sys, wantSys)
			}
			if wantUsr := VnetKey(tc.wantFmt + ".web"); usr != wantUsr {
				t.Errorf("user vnet: got %q, want %q", usr, wantUsr)
			}
		})
	}
}

func TestCanonicalVnetKey_OmittedNamespace_InfersLocal(t *testing.T) {
	r := &ResolutionReconciler{}
	const podNS = "app"
	if got, want := r.canonicalVnetKey(ref("web", ""), podNS), VnetKey("app.web"); got != want {
		t.Errorf("user vnet: got %q, want %q", got, want)
	}
	if got, want := r.canonicalVnetKey(ref(SystemVnetNamespace, ""), podNS), VnetKey("app."+SystemVnetNamespace); got != want {
		t.Errorf("namespace vnet: got %q, want %q", got, want)
	}
	// The cluster singleton's canonical key is bare (ADR 0033). Omitting the
	// namespace yields it directly; no operator-namespace lookup needed.
	if got, want := r.canonicalVnetKey(ref(SystemVnetCluster, ""), podNS), VnetKey(SystemVnetCluster); got != want {
		t.Errorf("cluster vnet: got %q, want %q", got, want)
	}
}

// An explicit cluster namespace is kept fully-qualified so Permits can verify
// it against the real CR (which exists only in the operator's namespace).
// It is collapsed to the bare canonical form only after permission passes.
func TestCanonicalVnetKey_ClusterVnet_ExplicitNamespace_StaysQualified(t *testing.T) {
	r := &ResolutionReconciler{}
	if got, want := r.canonicalVnetKey(ref(SystemVnetCluster, "kube-vnet-system"), "app"),
		VnetKey("kube-vnet-system."+SystemVnetCluster); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := r.canonicalVnetKey(ref(SystemVnetCluster, "bogus"), "app"),
		VnetKey("bogus."+SystemVnetCluster); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// REGRESSION LOCK. Permits short-circuited on the vnet NAME alone, so any
// `<ns>.cluster` key was permitted regardless of its home namespace — the
// reason a wrong `cluster` ref could never be caught. Only the bare canonical
// form may short-circuit.
func TestPermits_ClusterVnet_ForeignNamespace_NotPermitted(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(schemeForPermits(t)).
		WithObjects(
			mkNamespace("app", nil),
			// The real cluster vnet lives only in the operator's namespace.
			mkVnet(SystemVnetCluster, "kube-vnet-system", &vnetv1alpha1.NamespaceSelector{All: true}),
		).Build()

	ok, err := Permits(context.Background(), c, VnetKey("bogus."+SystemVnetCluster), "app")
	if err != nil {
		t.Fatalf("Permits: %v", err)
	}
	if ok {
		t.Fatal("bogus.cluster was permitted; a non-existent cluster vnet must be denied like any other not-found vnet")
	}
}

// The bare canonical form must still short-circuit to permitted — narrowing
// the short-circuit must not break the form every stamp/policy actually uses.
func TestPermits_ClusterVnet_BareKey_StillPermitted(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(schemeForPermits(t)).
		WithObjects(mkNamespace("app", nil)).Build()

	ok, err := Permits(context.Background(), c, VnetKey(SystemVnetCluster), "app")
	if err != nil {
		t.Fatalf("Permits: %v", err)
	}
	if !ok {
		t.Fatal("bare `cluster` key must remain permitted")
	}
}

// An explicit, correct cluster namespace resolves via a real Get against the
// operator-namespace CR (allowedNamespaces.All) — the singleton's home is
// discovered, never hardcoded.
func TestPermits_ClusterVnet_OperatorNamespace_Permitted(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(schemeForPermits(t)).
		WithObjects(
			mkNamespace("app", nil),
			mkVnet(SystemVnetCluster, "kube-vnet-system", &vnetv1alpha1.NamespaceSelector{All: true}),
		).Build()

	ok, err := Permits(context.Background(), c, VnetKey("kube-vnet-system."+SystemVnetCluster), "app")
	if err != nil {
		t.Fatalf("Permits: %v", err)
	}
	if !ok {
		t.Fatal("kube-vnet-system.cluster must be permitted (vnet exists, allowedNamespaces.All)")
	}
}

// notJoinableHint must be pure formatting: reserved names get a targeted
// suggestion, everything else gets nothing, and it never inspects cluster
// state. If this ever needs a client or a namespace, the per-kind
// special-casing ADR 0043 removed has crept back into control flow.
func TestNotJoinableHint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ref     vnetv1alpha1.VirtualNetworkRef
		wantSub string // "" = expect no hint at all
	}{
		{"cluster gets singleton hint", ref(SystemVnetCluster, "bogus"), "cluster-wide singleton"},
		{"namespace gets per-namespace hint", ref(SystemVnetNamespace, "kube-vnet-system"), "every managed namespace"},
		{"user vnet gets no hint", ref("web", "other"), ""},
		{"user vnet, omitted ns, no hint", ref("web", ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := notJoinableHint(tc.ref)
			if tc.wantSub == "" {
				if got != "" {
					t.Fatalf("expected no hint for a user vnet, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("hint %q does not mention %q", got, tc.wantSub)
			}
		})
	}
}

// The Event reason is the machine contract (alerts, --field-selector). It must
// be a single constant, never derived from the vnet kind.
func TestReasonVirtualNetworkNotJoinable_IsUniform(t *testing.T) {
	if ReasonVirtualNetworkNotJoinable != "VirtualNetworkNotJoinable" {
		t.Fatalf("reason changed: %q", ReasonVirtualNetworkNotJoinable)
	}
}

// bareJoinLabelHint carries the guidance folded in from the retired
// JoinLabelDiagnosticReconciler (ADR 0027): a bare `kube-vnet/net.<X>` that
// can't be honored should point the user at the prefixed form. Prefixed labels
// (already fully explained by notJoinableNote) and reserved system-vnet names
// (legitimately bare) get no hint.
func TestBareJoinLabelHint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		labelKey string
		suffix   string
		wantSub  string // "" = expect no hint
	}{
		{
			name:     "bare user vnet steers to prefixed form",
			labelKey: "kube-vnet/net.payments",
			suffix:   "payments",
			wantSub:  "kube-vnet/net.<homeNS>.payments",
		},
		{
			name:     "prefixed form gets no extra hint",
			labelKey: "kube-vnet/net.shop.payments",
			suffix:   "shop.payments",
			wantSub:  "",
		},
		{
			name:     "reserved cluster name is legitimately bare",
			labelKey: "kube-vnet/net.cluster",
			suffix:   SystemVnetCluster,
			wantSub:  "",
		},
		{
			name:     "reserved namespace name is legitimately bare",
			labelKey: "kube-vnet/net.namespace",
			suffix:   SystemVnetNamespace,
			wantSub:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bareJoinLabelHint(tc.labelKey, tc.suffix)
			if tc.wantSub == "" {
				if got != "" {
					t.Fatalf("expected no hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("hint %q does not mention %q", got, tc.wantSub)
			}
			if !strings.Contains(got, tc.labelKey) {
				t.Fatalf("hint %q should quote the offending label %q", got, tc.labelKey)
			}
		})
	}
}
