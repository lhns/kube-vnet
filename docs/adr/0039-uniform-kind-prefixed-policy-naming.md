# ADR 0039 — Uniform kind-prefixed naming for operator-emitted NetworkPolicies

**Status**: Accepted (2026-06-26)

## Context

Three kinds of NetworkPolicy are emitted by the operator today (with hostPort under ADR 0040 making a fourth source-subkind):

| Kind | Pre-0039 name shape |
|---|---|
| Baseline (one per managed NS) | literal `kube-vnet` |
| Cluster vnet membership | `kube-vnet.cluster-<8hex>` (bare per ADR 0033 amendment) |
| Namespaced vnet membership | `kube-vnet.<homeNS>.<vnet>-<8hex>` |
| External-allow (Service) | `kube-vnet.external-<svcName>-<8hex>` |

Problems:

1. **Kind isn't visible at a glance.** A reader sees `kube-vnet.headlamp.namespace-c0ed5701` and has to know the convention to decode `headlamp` = home-NS, `namespace` = vnet name. The shape is implicit in segment count.
2. **Inconsistent separators.** Membership uses dots; external-allow uses `external-` (hyphen). User-facing inconsistency for no good reason.
3. **No room for a second source-subkind.** ADR 0040 (hostPort auto-allow) adds host-port-source external-allow policies. Cramming both into one `external-` prefix means encoding the kind in the next segment (`external-svc-…` vs `external-pod-…`), which reads as a label-soup.

## Decision

Adopt a uniform `kube-vnet.<kind>.<identity>-<8hex>` shape across every operator-emitted policy.

```
kube-vnet.base                                — baseline (one per NS; no identity needed)
kube-vnet.mem.cluster-<8hex>                  — cluster vnet membership
kube-vnet.mem.<homeNS>.<vnet>-<8hex>          — namespaced vnet membership
kube-vnet.ext.svc.<svcName>-<8hex>            — Service-source external-allow
kube-vnet.ext.host.<port>.<proto>-<8hex>      — host-port-source external-allow (ADR 0040)
```

Three-letter kind slugs: `base`, `mem`, `ext`. The cluster vnet keeps its bare `cluster` identity inside the `mem.` prefix (ADR 0033 amendment preserved — no `<homeNS>.` segment for cluster because cluster has no home).

External-allow has two source kinds — `svc` and `host` — distinguished as the third dot-segment. There is no `ext.pod` kind: per-pod NetworkPolicy emission would churn on every pod rollout, so host-source policies are keyed on `(NS, port, protocol)` instead and select pods via stamped labels (see ADR 0040).

## Migration

The change is a name rename of every operator-emitted policy. Strategy:

1. **Reconcilers emit under the new names from the upgrade onwards.** SSA-apply with `FieldManager: kube-vnet` creates the new-named object.
2. **Per-reconciler migration tail-step removes the old-named policy** on every reconcile after upgrade. Idempotent and self-disabling — after the first sweep, the old object is gone and the delete becomes a no-op.
3. **For membership policies**, the existing `deleteStale` pass in `VirtualNetworkReconciler` naturally handles the rename: it lists all policies labeled with this vnet's `kube-vnet.system/network=<homeNS>.<vnet>`, then deletes anything not in the desired-name set. Old-format policies aren't in the new desired set, so they're swept without a dedicated migration step.
4. **For baseline + external-allow**, a one-line `r.Delete(legacyNamedPolicy)` follows the new-name apply.

The migration completes within one reconcile cycle of the new operator coming up. No manual `kubectl delete` is needed.

## Consequences

### Visible to users

- Every policy name changes shape on upgrade. **Label-driven tooling is unaffected** because the labels (`kube-vnet.system/managed-by`, `kube-vnet.system/role`, `kube-vnet.system/network`) are independent of name.
  - Cleanup hook (ADR 0036): unchanged — selects by `kube-vnet.system/managed-by=kube-vnet`.
  - System-labels VAP (ADR 0037): unchanged — matches by `kube-vnet.system/*` labels.
  - Verification queries (`kubectl get netpol -A -l kube-vnet.system/role=baseline`) — all unchanged.
- Documentation and tutorial examples that reference specific policy names get updated en masse (this PR's doc sweep covers the active corpus).
- Operators who maintain dashboards or alerting rules keyed on specific policy names need to update them. This is rare in practice; most observability rides on labels.

### Internal

- One more constant per kind in `policy_generator.go`; identity-hash function unchanged.
- Adding a new operator-emitted policy kind in the future has a clear extension point — pick a 3–4 letter slug, add it to the kind table, document the identity scheme.
- Test assertions on specific names need updating once (this PR).

## Alternatives considered

- **Keep current naming, contort hostPort into `external-pod-…` vs `external-svc-…`**. Rejected: the convention's already inconsistent (dot vs hyphen); doubling down on it makes the eventual cleanup more disruptive.
- **Adopt the new naming only for external-allow; leave membership and baseline alone**. Rejected: leaves the convention split half-and-half, perpetually confusing readers.
- **Use longer hash (12 or 16 hex) instead of renaming**. Orthogonal — the kind-prefix concern is about readability, not collision space. Can land independently if a user reports collision issues.
- **Encode kind into the resource name segment after `kube-vnet.`** (current `kube-vnet.<homeNS>.<vnet>`) **using a marker character**. Considered `kube-vnet:base`, `kube-vnet:mem.…` — rejected because `:` isn't a valid Kubernetes resource name character.

## Related ADRs

- **ADR 0006** — baseline default-deny. Name changes from literal `kube-vnet` to `kube-vnet.base`.
- **ADR 0011** — policy naming and truncation. Truncate-and-hash overflow handler still applies, just to a longer prefix.
- **ADR 0030** — unified vnet-membership. Membership shape changes to `mem.` prefix.
- **ADR 0033** — canonical FQ system labels. The amendment's bare-cluster naming is preserved within `mem.cluster`.
- **ADR 0036** — cleanup hook. Unchanged; label-based.
- **ADR 0037** — `kube-vnet.system/` prefix for operator-owned keys. Unchanged; labels independent of names.
- **ADR 0038** — external-allow Services. Name shape moves from `external-<svcName>` to `ext.svc.<svcName>`.
- **ADR 0040** — hostPort auto-allow. Uses the new `ext.host.<port>.<proto>` shape from the start.
