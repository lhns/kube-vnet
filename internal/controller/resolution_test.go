package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Three-tier baseline lattice (ADR 0031): ClusterBaseline → NamespaceBaseline
// → Pod (bindings + labels). Bare directions are enforced; default-* are
// override-able. Within-tier conflicts intersect.

func TestResolve_BaselinesOnly(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeClusterBaseline,
			Rules: []ResolutionRule{
				{Vnet: "namespace", Direction: DirectionDefaultBoth, Source: "ClusterVirtualNetworkBaseline/default"},
				{Vnet: "cluster", Direction: DirectionDefaultEgress, Source: "ClusterVirtualNetworkBaseline/default"},
			},
		},
	})
	if got := res.Effective["namespace"]; got != DirectionBoth {
		t.Errorf("namespace = %q, want both (default-* stripped)", got)
	}
	if got := res.Effective["cluster"]; got != DirectionEgress {
		t.Errorf("cluster = %q, want egress", got)
	}
	if len(res.Conflicts) != 0 || len(res.OverrideRejected) != 0 {
		t.Errorf("expected no conflicts/rejections, got %+v / %+v", res.Conflicts, res.OverrideRejected)
	}
}

func TestResolve_NamespaceBaselineOverridesCluster(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeClusterBaseline,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionDefaultEgress, Source: "cb"}},
		},
		{
			Scope: ScopeNamespaceBaseline,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionDefaultBoth, Source: "nb"}},
		},
	})
	if got := res.Effective["cluster"]; got != DirectionBoth {
		t.Errorf("cluster = %q, want both (NS baseline overrode default-egress)", got)
	}
	if len(res.OverrideRejected) != 0 {
		t.Errorf("override should be permitted (cluster used default-*); got rejections %+v", res.OverrideRejected)
	}
}

func TestResolve_OverrideRejectedWhenClusterBare(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeClusterBaseline,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionBoth, Source: "cb"}}, // BARE
		},
		{
			Scope: ScopeNamespaceBaseline,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionEgress, Source: "nb"}}, // try to narrow
		},
	})
	if got := res.Effective["cluster"]; got != DirectionBoth {
		t.Errorf("cluster = %q, want both (cluster baseline pinned bare)", got)
	}
	if len(res.OverrideRejected) != 1 {
		t.Fatalf("expected 1 override-rejected, got %d: %+v", len(res.OverrideRejected), res.OverrideRejected)
	}
	rj := res.OverrideRejected[0]
	if rj.Vnet != "cluster" || rj.AttemptedScope != ScopeNamespaceBaseline || rj.BlockingScope != ScopeClusterBaseline {
		t.Errorf("rejection shape unexpected: %+v", rj)
	}
}

// Restating the pinned value changes nothing, so it is not a rejected
// override. Reporting it would put a Warning on every pod that carries the
// same join label the cluster baseline pins.
func TestResolve_AgreeingWithBarePinIsNotRejected(t *testing.T) {
	for _, d := range []Direction{DirectionBoth, DirectionDefaultBoth} {
		res := Resolve([]ResolutionLayer{
			{
				Scope: ScopeClusterBaseline,
				Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionBoth, Source: "cb"}},
			},
			{
				Scope: ScopeNamespaceBaseline,
				Rules: []ResolutionRule{{Vnet: "cluster", Direction: d, Source: "nb"}},
			},
		})
		if got := res.Effective["cluster"]; got != DirectionBoth {
			t.Errorf("%s: cluster = %q, want both", d, got)
		}
		if len(res.OverrideRejected) != 0 {
			t.Errorf("%s: no override attempted, got rejections %+v", d, res.OverrideRejected)
		}
	}
}

func TestResolve_BindingOverridesNamespaceBaseline(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeNamespaceBaseline,
			Rules: []ResolutionRule{{Vnet: "x", Direction: DirectionDefaultEgress, Source: "nb"}},
		},
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{{Vnet: "x", Direction: DirectionBoth, Source: "VirtualNetworkBinding/y"}},
		},
	})
	if got := res.Effective["x"]; got != DirectionBoth {
		t.Errorf("x = %q, want both (binding overrode default-egress)", got)
	}
}

func TestResolve_NoneOptsOutOfInherited(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeClusterBaseline,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionDefaultBoth, Source: "cb"}},
		},
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionNone, Source: "<pod-label>"}},
		},
	})
	if _, ok := res.Effective["cluster"]; ok {
		t.Errorf("cluster should be removed by direction=none, got %q", res.Effective["cluster"])
	}
}

func TestResolve_BareNoneIsHardOptOut(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeClusterBaseline,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionNone, Source: "cb"}}, // bare none
		},
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionBoth, Source: "<pod-label>"}},
		},
	})
	if _, ok := res.Effective["cluster"]; ok {
		t.Errorf("cluster should remain off (bare none); got %q", res.Effective["cluster"])
	}
	if len(res.OverrideRejected) != 1 {
		t.Fatalf("expected pod's override to be rejected, got %+v", res.OverrideRejected)
	}
}

func TestResolve_LabelBindingConflictIntersection(t *testing.T) {
	// Pod has a binding setting ingress and a label setting egress for the
	// same vnet. Per ADR 0031, both are pod-tier siblings; intersection
	// gives effective=none → vnet is dropped.
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{
				{Vnet: "x", Direction: DirectionIngress, Source: "VirtualNetworkBinding/b"},
				{Vnet: "x", Direction: DirectionEgress, Source: "<pod-label>"},
			},
		},
	})
	if _, ok := res.Effective["x"]; ok {
		t.Errorf("x should drop out (intersection of ingress + egress = none); got %q", res.Effective["x"])
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(res.Conflicts), res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.Vnet != "x" || c.Scope != ScopePod || c.Effective != DirectionNone {
		t.Errorf("conflict shape unexpected: %+v", c)
	}
	if len(c.Participants) != 2 {
		t.Errorf("expected 2 participants, got %d", len(c.Participants))
	}
}

func TestResolve_TwoBindingsSamePodConflictIntersection(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{
				{Vnet: "x", Direction: DirectionBoth, Source: "VirtualNetworkBinding/a"},
				{Vnet: "x", Direction: DirectionEgress, Source: "VirtualNetworkBinding/b"},
			},
		},
	})
	if got := res.Effective["x"]; got != DirectionEgress {
		t.Errorf("x = %q, want egress (intersection of both + egress)", got)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(res.Conflicts))
	}
}

func TestResolve_IntersectionTruthTable(t *testing.T) {
	cases := []struct {
		a, b Direction
		want Direction
	}{
		{DirectionBoth, DirectionBoth, DirectionBoth},
		{DirectionBoth, DirectionIngress, DirectionIngress},
		{DirectionBoth, DirectionEgress, DirectionEgress},
		{DirectionBoth, DirectionNone, DirectionNone},
		{DirectionIngress, DirectionIngress, DirectionIngress},
		{DirectionIngress, DirectionEgress, DirectionNone},
		{DirectionIngress, DirectionNone, DirectionNone},
		{DirectionEgress, DirectionEgress, DirectionEgress},
		{DirectionEgress, DirectionNone, DirectionNone},
		{DirectionNone, DirectionNone, DirectionNone},
		// default-* inputs strip to bare equivalents.
		{DirectionDefaultBoth, DirectionEgress, DirectionEgress},
		{DirectionDefaultIngress, DirectionDefaultEgress, DirectionNone},
	}
	for _, tc := range cases {
		if got := intersect(tc.a, tc.b); got != tc.want {
			t.Errorf("intersect(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
		if got := intersect(tc.b, tc.a); got != tc.want {
			t.Errorf("intersect(%s, %s) = %s, want %s (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestResolve_EmptyInputs(t *testing.T) {
	res := Resolve(nil)
	if len(res.Effective) != 0 || len(res.Conflicts) != 0 || len(res.OverrideRejected) != 0 {
		t.Errorf("empty input should produce empty output, got %+v", res)
	}
}

func TestResolve_NoConflictWhenSameDirection(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{
				{Vnet: "x", Direction: DirectionBoth, Source: "a"},
				{Vnet: "x", Direction: DirectionBoth, Source: "b"},
			},
		},
	})
	if got := res.Effective["x"]; got != DirectionBoth {
		t.Errorf("x = %q, want both", got)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("matching directions should not surface as conflict: %+v", res.Conflicts)
	}
}

func TestResolve_DeterministicConflictOrder(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopePod,
			Rules: []ResolutionRule{
				{Vnet: "z", Direction: DirectionBoth, Source: "a"},
				{Vnet: "z", Direction: DirectionEgress, Source: "b"},
			},
		},
		{
			Scope: ScopeNamespaceBaseline,
			Rules: []ResolutionRule{
				{Vnet: "y", Direction: DirectionDefaultBoth, Source: "a"},
				{Vnet: "y", Direction: DirectionDefaultIngress, Source: "b"},
			},
		},
	})
	if len(res.Conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(res.Conflicts))
	}
	// Lower scope (NamespaceBaseline) sorts first.
	if res.Conflicts[0].Scope != ScopeNamespaceBaseline || res.Conflicts[0].Vnet != "y" {
		t.Errorf("first conflict = %+v, want NamespaceBaseline/y", res.Conflicts[0])
	}
	if res.Conflicts[1].Scope != ScopePod || res.Conflicts[1].Vnet != "z" {
		t.Errorf("second conflict = %+v, want Pod/z", res.Conflicts[1])
	}
}

// Cluster-singleton inversion (ADR 0033 Amendment): bare `cluster` stays bare,
// it never gains the pod's namespace.
func TestCanonicalSuffix_ClusterStaysBare(t *testing.T) {
	if got := CanonicalSuffix("cluster", "platform"); got != "cluster" {
		t.Errorf("got %q, want cluster", got)
	}
}

// A prefixed `<X>.cluster` no longer collapses to the singleton (ADR 0033,
// 2026-09-25 amendment): the pod-label path rejects it before canonicalizing,
// and anything else prefixed passes through untouched.
func TestCanonicalSuffix_PrefixedClusterDoesNotCollapse(t *testing.T) {
	for _, in := range []string{"kube-vnet-system.cluster", "random.cluster"} {
		if got := CanonicalSuffix(in, "platform"); got == "cluster" {
			t.Errorf("CanonicalSuffix(%q) collapsed to bare cluster", in)
		}
	}
}

// Every namespaced cluster join label is dropped with a VirtualNetworkNotJoinable
// Warning on the pod, including the operator's own namespace; the bare form
// still joins.
func TestPodLabelRules_PrefixedClusterLabelRejected(t *testing.T) {
	rec := &fakeRecorder{}
	r := &Resolver{Recorder: rec}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "p",
		Labels: map[string]string{
			"kube-vnet/net.kube-vnet-system.cluster": "both",
			"kube-vnet/net.shop.cluster":             "ingress",
		},
	}}

	if rules := r.podLabelRules(pod); len(rules) != 0 {
		t.Fatalf("prefixed cluster labels must not produce rules, got %v", rules)
	}
	notes := rec.on(ReasonVirtualNetworkNotJoinable, &corev1.Pod{}, "shop", "p")
	if len(notes) != 2 {
		t.Fatalf("want 2 %s Warnings on the pod, got %d (reasons %v)",
			ReasonVirtualNetworkNotJoinable, len(notes), rec.reasons)
	}
	for i, typ := range rec.types {
		if typ != corev1.EventTypeWarning {
			t.Errorf("event %d type = %q, want Warning", i, typ)
		}
	}
	for _, n := range notes {
		if !strings.Contains(n, "the cluster network has no namespace; use kube-vnet/net.cluster") {
			t.Errorf("note %q does not steer to the bare label", n)
		}
	}

	pod.Labels = map[string]string{"kube-vnet/net.cluster": "both"}
	rules := r.podLabelRules(pod)
	if len(rules) != 1 || rules[0].Vnet != VnetKey(SystemVnetCluster) {
		t.Fatalf("bare cluster label must still join as `cluster`, got %v", rules)
	}
}

// A virtualNetworkRef that names the cluster vnet's real home namespace is
// legitimate (ADR 0043) and still stamps the bare singleton key.
func TestStampedVnetKey(t *testing.T) {
	for in, want := range map[VnetKey]VnetKey{
		"cluster":                  "cluster",
		"kube-vnet-system.cluster": "cluster",
		"shop.payments":            "shop.payments",
		"shop.namespace":           "shop.namespace",
	} {
		if got := stampedVnetKey(in); got != want {
			t.Errorf("stampedVnetKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalSuffix_NamespaceFollowsBareRule(t *testing.T) {
	if got := CanonicalSuffix("namespace", "platform"); got != "platform.namespace" {
		t.Errorf("got %q, want platform.namespace", got)
	}
}

func TestCanonicalSuffix_BareUserVnetFollowsBareRule(t *testing.T) {
	if got := CanonicalSuffix("payments", "platform"); got != "platform.payments" {
		t.Errorf("got %q, want platform.payments", got)
	}
}

func TestCanonicalSuffix_FQUserVnetPassThrough(t *testing.T) {
	if got := CanonicalSuffix("monitoring.observability", "platform"); got != "monitoring.observability" {
		t.Errorf("got %q, want monitoring.observability", got)
	}
}
