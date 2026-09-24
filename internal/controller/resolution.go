package controller

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// VnetKey identifies a vnet in resolution: `<homeNS>.<vnet>`, or bare
// `cluster` for the cluster singleton (ADR 0033). The canonical key is also
// the suffix of the operator's `kube-vnet.system/net.` stamp.
//
// Resolve treats VnetKey as opaque; callers compute the key for each rule.
type VnetKey string

// Display renders k for users as `<homeNS>/<name>`, the form every message
// uses; the cluster singleton is `cluster`.
func (k VnetKey) Display() string {
	homeNS, name, ok := splitVnetKey(k)
	if !ok || homeNS == "" {
		return string(k)
	}
	return homeNS + "/" + name
}

// ResolutionRule is one row of the inheritance lattice — a single baseline
// membership, binding, or pod-label entry that contributes to the effective
// state. Source names the contributing object (e.g.
// "VirtualNetworkBinding/<name>" or "pod label kube-vnet/net.<x>"); it
// appears in conflict reports.
type ResolutionRule struct {
	Vnet      VnetKey
	Direction Direction
	Source    string

	// Ref is the VirtualNetworkRef this rule was built from, retained only so
	// a not-joinable diagnostic can quote what the user actually wrote and
	// hint at the fix (ADR 0043). Zero for rules derived from pod join labels.
	Ref vnetv1alpha1.VirtualNetworkRef

	// Hint is an optional extra sentence appended to the VirtualNetworkNotJoinable
	// Warning when this rule is dropped, carrying source-specific guidance a
	// generic message can't. Pod join labels set it to steer a bare
	// `kube-vnet/net.<X>` toward the prefixed form when no local vnet X exists.
	Hint string

	// Owner is the object that declared this rule — the namespace Baseline,
	// Binding, or the Pod itself for join labels and for the cluster-scoped
	// baseline (whose Events would land in `default`). A VirtualNetworkNotJoinable Warning
	// Event is emitted on it when the rule is dropped. Never used for
	// resolution logic.
	Owner client.Object
}

// ResolutionScope identifies the tier a rule comes from. Tiers cascade in
// numeric order (lowest first); the cascade respects override-permission
// encoded in the upstream tier's Direction value (bare = enforced;
// default-* = override-able). See ADR 0031.
type ResolutionScope int

const (
	// ScopeClusterBaseline is the cluster-wide tier-default
	// (ClusterVirtualNetworkBaseline, singleton named `default`).
	ScopeClusterBaseline ResolutionScope = iota
	// ScopeNamespaceBaseline is the namespace-wide tier-default
	// (VirtualNetworkBaseline, singleton per namespace named `default`).
	ScopeNamespaceBaseline
	// ScopePod groups workload-specific sources: VirtualNetworkBindings that
	// match the pod, plus the pod's own `kube-vnet/net.<vnet>=<dir>` labels.
	// All sources within this scope are intersected (fail-closed) on
	// conflict, per ADR 0031. Bindings and labels are siblings in authority
	// terms; pod authors and namespace-admins co-own this tier.
	ScopePod
)

func (s ResolutionScope) String() string {
	switch s {
	case ScopeClusterBaseline:
		return "cluster-baseline"
	case ScopeNamespaceBaseline:
		return "namespace-baseline"
	case ScopePod:
		return "pod"
	}
	return fmt.Sprintf("scope-%d", int(s))
}

// describe names the tier's sources for users.
func (s ResolutionScope) describe() string {
	switch s {
	case ScopeClusterBaseline:
		return "the ClusterVirtualNetworkBaseline"
	case ScopeNamespaceBaseline:
		return "this namespace's VirtualNetworkBaseline"
	case ScopePod:
		return "a binding or pod label"
	}
	return s.String()
}

// ResolutionLayer is all rules from a single scope. The resolver iterates
// scopes from lowest to highest priority.
type ResolutionLayer struct {
	Scope ResolutionScope
	Rules []ResolutionRule
}

// ResolutionConflict surfaces when two rules within the same scope disagree
// on direction for the same vnet. Under intersection semantics (ADR 0031)
// the conflict still produces a deterministic effective direction — the
// intersection of all participating directions. The report (a
// ResolutionConflict Warning on the pod) is for humans; the effective
// direction is correct without it.
type ResolutionConflict struct {
	Vnet         VnetKey
	Scope        ResolutionScope
	Participants []ResolutionRule // every rule that disagreed
	Effective    Direction        // result of intersection (bare; default-* prefix consumed)
}

// OverrideRejected records a downstream tier's attempt to override a vnet
// where the upstream tier had pinned a bare (enforced) direction. The
// downstream attempt is ignored; the upstream value remains effective.
type OverrideRejected struct {
	Vnet           VnetKey
	AttemptedScope ResolutionScope // scope whose attempt was rejected
	AttemptedDir   Direction       // the would-be direction
	BlockingScope  ResolutionScope // scope that pinned the bare value
	BlockingDir    Direction       // the bare value blocking override
}

// ResolutionResult is the resolved effective state for one pod.
type ResolutionResult struct {
	// Effective is the desired set of (vnet, direction) pairs. Directions are
	// always bare — the default-* prefix is consumed during resolution.
	// Entries with effective Direction=none are left out.
	Effective map[VnetKey]Direction

	// Conflicts records same-scope disagreements that were resolved via
	// intersection. The effective direction is recorded on each conflict.
	Conflicts []ResolutionConflict

	// OverrideRejected records cross-tier rejections (bare upstream blocked
	// downstream override). One entry per (scope, vnet) attempt.
	OverrideRejected []OverrideRejected
}

// intersect returns the intersection of two directions. Both inputs are
// reduced to their bare components first; the result is always bare.
//
// Truth table (ADR 0031):
//
//	          both   ingress egress  none
//	both    | both   ingress egress  none
//	ingress | ingress ingress none    none
//	egress  | egress  none    egress  none
//	none    | none    none    none    none
//
// Symmetric. Any participant of `none` zeroes the result. Differing
// single-direction values (ingress vs egress) collapse to none.
func intersect(a, b Direction) Direction {
	a, b = a.Bare(), b.Bare()
	if a == DirectionNone || b == DirectionNone {
		return DirectionNone
	}
	if a == b {
		return a
	}
	// At this point a != b and neither is none. Possible pairs (unordered):
	// {both,ingress} → ingress; {both,egress} → egress; {ingress,egress} → none.
	if a == DirectionBoth {
		return b
	}
	if b == DirectionBoth {
		return a
	}
	// {ingress, egress}
	return DirectionNone
}

// Resolve combines the layers in priority order and returns the effective
// state. Pure function — no I/O, no globals — so callers can build inputs
// from any source (controllers, tests, dry-run tools).
//
// Semantics (ADR 0031):
//   - Within a layer, rules with the same Vnet are merged via intersection.
//     Disagreeing rules surface as a Conflict with the resulting bare
//     direction; same-direction rules don't conflict.
//   - Across layers, lower-scope (upstream) values cascade to higher-scope
//     (downstream) tiers unless the downstream tier provides its own value.
//     Override-permission is per-vnet: bare upstream values cannot be
//     overridden (downstream attempts are recorded in OverrideRejected and
//     ignored); default-* upstream values may be overridden.
//   - The final emitted direction is always bare; default-* prefix is
//     consumed during resolution to compute override-permission.
//   - Entries with effective Direction=none are dropped from the result.
func Resolve(layers []ResolutionLayer) ResolutionResult {
	ordered := slices.Clone(layers)
	slices.SortStableFunc(ordered, func(a, b ResolutionLayer) int { return cmp.Compare(a.Scope, b.Scope) })

	// running carries the cascade value (still default-*-tagged so cross-tier
	// override-permission can be evaluated). Stripped to bare at the end.
	type running struct {
		Direction Direction
		FromScope ResolutionScope
	}
	current := map[VnetKey]running{}

	var conflicts []ResolutionConflict
	var rejected []OverrideRejected

	for _, layer := range ordered {
		// Within-layer: group by Vnet, intersect across same-vnet rules.
		byVnet := map[VnetKey][]ResolutionRule{}
		for _, r := range layer.Rules {
			byVnet[r.Vnet] = append(byVnet[r.Vnet], r)
		}
		// Sorted for deterministic conflict reporting.
		for _, vnet := range slices.Sorted(maps.Keys(byVnet)) {
			rules := byVnet[vnet]
			slices.SortStableFunc(rules, func(a, b ResolutionRule) int { return strings.Compare(a.Source, b.Source) })

			// Intersect all directions in the group.
			eff := rules[0].Direction
			disagree := false
			anyDefault := rules[0].Direction.IsDefault()
			anyBare := !rules[0].Direction.IsDefault()
			for _, r := range rules[1:] {
				if r.Direction.Bare() != eff.Bare() {
					disagree = true
				}
				eff = intersect(eff, r.Direction)
				if r.Direction.IsDefault() {
					anyDefault = true
				} else {
					anyBare = true
				}
			}
			// The intersection result `eff` is bare. Re-apply default-* prefix
			// when all participants in this layer were default-*; otherwise the
			// stricter bare form propagates to the next tier.
			if anyDefault && !anyBare {
				eff = "default-" + eff.Bare()
			}

			if disagree {
				conflicts = append(conflicts, ResolutionConflict{
					Vnet:         vnet,
					Scope:        layer.Scope,
					Participants: append([]ResolutionRule(nil), rules...),
					Effective:    eff.Bare(),
				})
			}

			// Cross-layer: apply this layer's effective value, respecting
			// upstream bare-pin if any. Restating the pinned value is not an
			// override.
			if existing, ok := current[vnet]; ok && !existing.Direction.IsDefault() {
				if eff.Bare() == existing.Direction {
					continue
				}
				rejected = append(rejected, OverrideRejected{
					Vnet:           vnet,
					AttemptedScope: layer.Scope,
					AttemptedDir:   eff,
					BlockingScope:  existing.FromScope,
					BlockingDir:    existing.Direction,
				})
				continue
			}
			current[vnet] = running{Direction: eff, FromScope: layer.Scope}
		}
	}

	// Strip default-* prefix and drop none entries.
	effective := map[VnetKey]Direction{}
	for vnet, r := range current {
		bare := r.Direction.Bare()
		if bare == DirectionNone {
			continue
		}
		effective[vnet] = bare
	}

	// Stable conflict + rejection ordering.
	slices.SortStableFunc(conflicts, func(a, b ResolutionConflict) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Vnet, b.Vnet))
	})
	slices.SortStableFunc(rejected, func(a, b OverrideRejected) int {
		return cmp.Or(cmp.Compare(a.AttemptedScope, b.AttemptedScope), cmp.Compare(a.Vnet, b.Vnet))
	})

	return ResolutionResult{
		Effective:        effective,
		Conflicts:        conflicts,
		OverrideRejected: rejected,
	}
}
