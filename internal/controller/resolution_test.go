package controller

import (
	"testing"
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
