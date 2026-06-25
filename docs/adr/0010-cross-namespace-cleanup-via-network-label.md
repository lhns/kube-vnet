# 0010 — Cross-namespace cleanup via the network label

Status: Accepted (cleanup label key renamed from `kube-vnet/network` to `kube-vnet.system/network` per [ADR 0037](0037-system-prefix-convention-for-operator-owned-keys.md); the cross-namespace cleanup mechanism described here is unchanged)

## Context

When a VirtualNetwork is deleted, every `NetworkPolicy` it generated must be removed — including those in foreign namespaces (when `allowedNamespaces` permitted cross-namespace joiners). Kubernetes' standard cleanup mechanism is owner references with garbage collection, but **owner references must point to a resource in the same namespace** (or a cluster-scoped resource). A namespaced VirtualNetwork in `monitoring` cannot own a `NetworkPolicy` in `platform` via owner references.

## Decision

- For policies in the home namespace, set an `OwnerReference` to the parent VirtualNetwork. Kubernetes garbage collection handles deletion automatically.
- For policies in foreign namespaces, the operator manages cleanup manually. Every operator-managed policy carries the label:

  ```yaml
  metadata:
    labels:
      kube-vnet.system/managed-by: kube-vnet
      kube-vnet.system/network: <homeNamespace>.<vnetName>
  ```

- On VirtualNetwork deletion (or 404 on a Get during reconcile), the operator lists all policies cluster-wide matching `kube-vnet.system/network=<home>.<name>` and deletes them.
- The same label is the source of truth for stale-policy detection during normal reconciliation: list by label, diff against the desired set, delete the difference.

## Consequences

- **Pro**: Cross-namespace cleanup is robust without requiring privileged tricks or finalizers in foreign namespaces.
- **Pro**: A label-list query is cheap and uses the existing watch cache.
- **Con**: If the operator is offline when a VirtualNetwork is deleted, foreign-namespace policies linger until the next reconcile of that VirtualNetwork (which won't happen if it's already gone). Mitigated by the periodic 10-minute resync detecting the 404 and triggering cleanup. Long offline windows could leak policies; envtest can cover this (deferred per ADR 0014).
- **Convention**: User-applied policies must NOT carry the `kube-vnet.system/managed-by=kube-vnet` label, or the operator will treat them as its own and delete them. Documented in the README.
