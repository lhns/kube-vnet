package controller

import (
	"fmt"
	"sort"
)

// VnetKey is the label suffix that goes after `kube-vnet/net.` (user input)
// or `kube-vnet.system/net.` (operator output). For the system vnets it's
// just `"namespace"` or `"cluster"`. For user vnets it follows the existing
// bare/prefixed convention: bare `"<vnet>"` when pod and vnet live in the
// same namespace, prefixed `"<homeNS>.<vnet>"` otherwise.
//
// Resolution treats VnetKey as opaque — it's the caller's job to compute the
// right key for each (binding, pod) pair before invoking the resolver.
type VnetKey string

// ResolutionRule is one row of the inheritance lattice — a single binding or
// pod-label entry that contributes to the effective state. Source is the
// binding's name (for bindings) or "<pod-label>" for direct pod labels;
// it's used as the alphabetical tiebreaker on within-scope conflicts and
// surfaced on the output's Conflicts list.
type ResolutionRule struct {
	Vnet      VnetKey
	Direction Direction
	Source    string
}

// ResolutionScope is the layer a rule comes from. Lower numeric value = lower
// priority. Higher value wins on cross-scope override.
type ResolutionScope int

const (
	ScopeOperatorDefault ResolutionScope = iota
	ScopeClusterBinding
	ScopeNamespaceBinding
	ScopePodLabel
)

func (s ResolutionScope) String() string {
	switch s {
	case ScopeOperatorDefault:
		return "operator-default"
	case ScopeClusterBinding:
		return "cluster-binding"
	case ScopeNamespaceBinding:
		return "namespace-binding"
	case ScopePodLabel:
		return "pod-label"
	}
	return fmt.Sprintf("scope-%d", int(s))
}

// ResolutionLayer is all rules from a single scope. The resolver iterates
// scopes from lowest to highest priority.
type ResolutionLayer struct {
	Scope ResolutionScope
	Rules []ResolutionRule
}

// ResolutionConflict surfaces when two rules in the *same* scope disagree
// on the direction for the same vnet. The alphabetical-by-source tiebreaker
// has been applied already; Winner is the rule that won, Losers are the
// rules that didn't.
type ResolutionConflict struct {
	Vnet    VnetKey
	Scope   ResolutionScope
	Winner  ResolutionRule
	Losers  []ResolutionRule
}

// ResolutionResult is the resolved effective state for one pod.
type ResolutionResult struct {
	// Effective is the desired set of (vnet, direction) pairs. Entries with
	// Direction=none are EXCLUDED — the resolver applies `none` as a removal,
	// not as a stored value, so this map only contains active memberships.
	Effective map[VnetKey]Direction

	// Conflicts records any same-scope disagreements that were resolved by
	// the alphabetical tiebreaker. Empty if no conflicts.
	Conflicts []ResolutionConflict
}

// Resolve combines the layers in priority order (lowest to highest) and
// returns the effective state. The function is pure — no I/O, no globals,
// no clock dependence — so callers can build inputs from any source
// (controllers, tests, dry-run tools).
//
// Semantics:
//   - Within a layer, rules with the same Vnet are deduplicated alphabetically
//     by Source: the lowest source name wins, others are recorded as conflicts.
//     Rules with the same direction don't conflict (no surprise).
//   - Across layers, higher scope overrides lower. Direction=none in any layer
//     suppresses inheritance — the vnet drops out of the effective map.
//   - Direction=none in the lowest layer where the vnet first appears just
//     means "not a member"; same observable result.
func Resolve(layers []ResolutionLayer) ResolutionResult {
	// Sort layers ascending by scope so iteration order is well-defined.
	ordered := make([]ResolutionLayer, len(layers))
	copy(ordered, layers)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Scope < ordered[j].Scope
	})

	effective := map[VnetKey]Direction{}
	var conflicts []ResolutionConflict

	for _, layer := range ordered {
		// Group this layer's rules by Vnet.
		byVnet := map[VnetKey][]ResolutionRule{}
		for _, r := range layer.Rules {
			byVnet[r.Vnet] = append(byVnet[r.Vnet], r)
		}

		// Within each Vnet, pick the alphabetical-by-Source winner. Record
		// conflicts when remaining rules disagree on direction.
		for vnet, rules := range byVnet {
			sort.SliceStable(rules, func(i, j int) bool {
				return rules[i].Source < rules[j].Source
			})
			winner := rules[0]
			var losers []ResolutionRule
			for _, r := range rules[1:] {
				if r.Direction != winner.Direction {
					losers = append(losers, r)
				}
			}
			if len(losers) > 0 {
				conflicts = append(conflicts, ResolutionConflict{
					Vnet:   vnet,
					Scope:  layer.Scope,
					Winner: winner,
					Losers: losers,
				})
			}

			// Apply the winner to the effective map: a non-none direction
			// adds/overrides; `none` removes any inherited entry.
			if winner.Direction == DirectionNone {
				delete(effective, vnet)
			} else {
				effective[vnet] = winner.Direction
			}
		}
	}

	// Stable conflict ordering for deterministic output.
	sort.SliceStable(conflicts, func(i, j int) bool {
		if conflicts[i].Scope != conflicts[j].Scope {
			return conflicts[i].Scope < conflicts[j].Scope
		}
		return conflicts[i].Vnet < conflicts[j].Vnet
	})

	return ResolutionResult{
		Effective: effective,
		Conflicts: conflicts,
	}
}
