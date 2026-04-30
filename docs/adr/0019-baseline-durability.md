# 0019 — Baseline durability via drift correction; AdminNetworkPolicy deferred

Status: Accepted

## Context

The `kube-vnet-default-deny` baseline (ADR 0006) is the policy that makes membership semantics meaningful — without it, pods that *aren't* members of a VirtualNetwork still reach members because Kubernetes' default is allow-all.

The baseline is a `networking.k8s.io/v1` `NetworkPolicy` that lives **in the namespace it protects**. That means anyone with `delete networkpolicy` RBAC in that namespace — which most application teams have for their own namespaces — can `kubectl delete networkpolicy kube-vnet-default-deny` and instantly restore default-allow. The threat is real: a user (or a misbehaving pipeline) can defeat kube-vnet's isolation in their own namespace simply by removing the deny policy.

The natural intuition is "put the deny rule in another namespace where the app team can't touch it." Stock `NetworkPolicy` doesn't allow this — its `podSelector` only matches pods in the policy's own namespace. A policy in `kube-vnet-system` cannot select or deny traffic in `platform`. So the rule has to be in the same namespace as the pods it protects, and namespace-level RBAC governs it.

## Decision

For v1alpha1, **lean on drift correction**:

- The reconciler already watches operator-managed `NetworkPolicy` resources (predicate filters on `kube-vnet/managed-by=kube-vnet`).
- A delete event fires the policy → vnet mapping (`policyToVNet`) and enqueues the owning VirtualNetwork.
- The reconciler re-applies the desired policy via SSA on the next reconcile pass.
- Window between deletion and restore is sub-second to a few seconds in practice.

Add observability so the defense is *visible* rather than silent:

- Before each apply, the reconciler reads the policy via the uncached `APIReader` (avoiding informer staleness). If it's absent immediately before the apply, that's evidence of an out-of-band deletion; the operator emits a `Warning PolicyRestored` event on the owning VirtualNetwork after the apply.
- This makes `kubectl describe vnet` and any event-aggregation pipeline (Datadog, Prometheus event exporter, Grafana, Slack alerts) surface the deletion-and-restore cycle clearly. A persistent attacker would generate a continuous stream of `PolicyRestored` events.

## Consequences

- **Pro**: No new dependencies. Works on any `NetworkPolicy`-enforcing CNI we already support.
- **Pro**: Covers the dominant threat: accidental deletion by a user or by a tool that's unaware of kube-vnet.
- **Pro**: An attacker needs to *outpace* the operator's reconcile loop to keep the deny rule absent. For sustained attacks they'd need a controller of their own; the resulting event flood would be obvious.
- **Con**: Not a hard guarantee. Between deletion and restore, traffic that the baseline would have denied is allowed. Window is small but non-zero.
- **Con**: While the operator is offline (rolling restart, leader-election failover, version mismatch), there's no protection at all.
- **Con**: An attacker with cluster-scoped privileges who can disable the operator entirely defeats the protection. (This threat is out of scope for kube-vnet; cluster admin compromise is its own catastrophe.)

## Future path: AdminNetworkPolicy

The proper Kubernetes-native answer is `policy.networking.k8s.io/v1 AdminNetworkPolicy` (ANP):

- **Cluster-scoped**: lives outside any namespace. Namespace-level RBAC has no authority over it.
- **Distinct API group**: ANP delete permission is granted separately from NP, so cluster admins can grant NP RBAC widely while keeping ANP locked down.
- **Higher precedence than NP**: an ANP `Deny` overrides any matching NP `Allow`. The deny baseline becomes a hard guarantee, not a reconciliation race.

Migration path when adopted:

- The single `kube-vnet-default-deny` per namespace becomes one cluster-scoped `AdminNetworkPolicy` whose `subject` selects every namespace the operator is permitted to act on (excluded list excluded, `kube-vnet/disabled` annotation excluded).
- Per-vnet membership policies remain stock `NetworkPolicy`. ANP precedence rules let a stock NP `Allow` permit traffic *in spite of* the cluster-wide ANP deny — but only for traffic the ANP doesn't actively `Deny`. Designing the ANP rule set to allow this composition correctly is the substance of the future ADR.
- The user-facing surface (`VirtualNetwork`, `allowedNamespaces`, the join labels) does not change.

Caveats holding ANP back from this PR:

- **CNI support varies**: Calico 3.28+, Cilium 1.16+, Antrea recent versions support ANP. kube-router does not (at time of writing). Adopting ANP would raise our CNI floor or require a dual-mode operator.
- **API maturity**: ANP API is `v1alpha1` / `v1beta1` depending on Kubernetes version. We'd be tracking a moving target.
- **Conflicts with ADR 0002**: that ADR established "standard `networking.k8s.io/v1` `NetworkPolicy` only." ANP is a different API group. Adopting it requires a superseding ADR.
- **Drift correction is enough for the dominant threat**. Accidental deletion, unaware tooling, and most hostile-but-not-malicious cases are covered.

## Cross-references

- ADR 0002 — "Emit standard NetworkPolicy only." Adopting ANP would update this.
- ADR 0006 — "Default-deny baseline." Defines what we're protecting.
- ADR 0014 — "Deferred v1 items." Cross-referenced; ANP adoption joins the deferred list.
- ADR 0016 — "Emit events on condition transitions." `PolicyRestored` follows the same eventing pattern (Warning event for noteworthy state change).
