package controller

import (
	"testing"
)

func TestResolve_OperatorDefaultsOnly(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeOperatorDefault,
			Rules: []ResolutionRule{
				{Vnet: "namespace", Direction: DirectionBoth, Source: "operator"},
				{Vnet: "cluster", Direction: DirectionEgress, Source: "operator"},
			},
		},
	})
	if got := res.Effective["namespace"]; got != DirectionBoth {
		t.Errorf("namespace = %q, want both", got)
	}
	if got := res.Effective["cluster"]; got != DirectionEgress {
		t.Errorf("cluster = %q, want egress", got)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %v, want none", res.Conflicts)
	}
}

func TestResolve_HigherScopeOverridesLower(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeOperatorDefault,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionEgress, Source: "operator"}},
		},
		{
			Scope: ScopeClusterBinding,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionBoth, Source: "cvnb-x"}},
		},
		{
			Scope: ScopeNamespaceBinding,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionIngress, Source: "vnb-y"}},
		},
		{
			Scope: ScopePodLabel,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionEgress, Source: "<pod-label>"}},
		},
	})
	if got := res.Effective["cluster"]; got != DirectionEgress {
		t.Errorf("highest-scope wins: cluster = %q, want egress", got)
	}
}

func TestResolve_NoneOptsOutOfInherited(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeOperatorDefault,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionBoth, Source: "operator"}},
		},
		{
			Scope: ScopePodLabel,
			Rules: []ResolutionRule{{Vnet: "cluster", Direction: DirectionNone, Source: "<pod-label>"}},
		},
	})
	if _, ok := res.Effective["cluster"]; ok {
		t.Errorf("cluster should be removed by direction=none, got %q", res.Effective["cluster"])
	}
}

func TestResolve_ConflictAlphabeticalTiebreaker(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeNamespaceBinding,
			Rules: []ResolutionRule{
				{Vnet: "payments", Direction: DirectionIngress, Source: "b-deny"},
				{Vnet: "payments", Direction: DirectionBoth, Source: "a-allow"},
			},
		},
	})
	if got := res.Effective["payments"]; got != DirectionBoth {
		t.Errorf("alphabetical winner: payments = %q, want both (from a-allow)", got)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(res.Conflicts), res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.Vnet != "payments" || c.Winner.Source != "a-allow" || len(c.Losers) != 1 || c.Losers[0].Source != "b-deny" {
		t.Errorf("conflict shape unexpected: %+v", c)
	}
}

func TestResolve_NoConflictWhenSameDirection(t *testing.T) {
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeNamespaceBinding,
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

func TestResolve_PodLabelOverridesBindingSameScope(t *testing.T) {
	// Pod label is its own scope — it always wins over bindings in any
	// other scope. Within the pod-label scope, conflicts can't really
	// happen because labels are key-unique on a pod.
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopeNamespaceBinding,
			Rules: []ResolutionRule{{Vnet: "x", Direction: DirectionBoth, Source: "vnb"}},
		},
		{
			Scope: ScopePodLabel,
			Rules: []ResolutionRule{{Vnet: "x", Direction: DirectionEgress, Source: "<pod-label>"}},
		},
	})
	if got := res.Effective["x"]; got != DirectionEgress {
		t.Errorf("x = %q, want egress (pod label wins)", got)
	}
}

func TestResolve_EmptyInputs(t *testing.T) {
	res := Resolve(nil)
	if len(res.Effective) != 0 || len(res.Conflicts) != 0 {
		t.Errorf("empty input should produce empty output, got %+v", res)
	}
}

func TestResolve_DeterministicConflictOrder(t *testing.T) {
	// Two conflicts, in two different scopes. Output should be sorted by
	// scope first, then by Vnet alphabetically.
	res := Resolve([]ResolutionLayer{
		{
			Scope: ScopePodLabel,
			Rules: []ResolutionRule{
				{Vnet: "z", Direction: DirectionBoth, Source: "a"},
				{Vnet: "z", Direction: DirectionEgress, Source: "b"},
			},
		},
		{
			Scope: ScopeNamespaceBinding,
			Rules: []ResolutionRule{
				{Vnet: "y", Direction: DirectionBoth, Source: "a"},
				{Vnet: "y", Direction: DirectionIngress, Source: "b"},
			},
		},
	})
	if len(res.Conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(res.Conflicts))
	}
	// Lower scope (NamespaceBinding) sorts first.
	if res.Conflicts[0].Scope != ScopeNamespaceBinding || res.Conflicts[0].Vnet != "y" {
		t.Errorf("first conflict = %+v, want NamespaceBinding/y", res.Conflicts[0])
	}
	if res.Conflicts[1].Scope != ScopePodLabel || res.Conflicts[1].Vnet != "z" {
		t.Errorf("second conflict = %+v, want PodLabel/z", res.Conflicts[1])
	}
}
