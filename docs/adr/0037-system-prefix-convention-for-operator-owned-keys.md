# 0037 — `kube-vnet.system/` prefix convention for operator-owned keys

Status: Accepted

Date: 2026-06-25

Refines / generalizes: [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (introduced `kube-vnet.system/` for stamped pod labels). Renames the cleanup label from [ADR 0010](0010-cross-namespace-cleanup-via-network-label.md).

## Context

ADR 0030 established `kube-vnet.system/net.*` and `kube-vnet.system/resolved-generation` as the prefix for operator-stamped pod labels: a namespace separate from the user-facing `kube-vnet/` keys, protected by a `ValidatingAdmissionPolicy` from user mutation. The convention was never generalized to the operator's other output keys.

Four operator-managed sentinels stayed under the user prefix:

| Key | On | Role |
|---|---|---|
| `kube-vnet/managed-by=kube-vnet` | every operator-emitted `NetworkPolicy` + operator-created `VirtualNetwork` | operator-ownership claim; cleanup selector |
| `kube-vnet/network=<homeNS>.<vnet>` | every membership policy | identifies the owning VirtualNetwork; cross-namespace cleanup (ADR 0010) |
| `kube-vnet/role=membership\|baseline` | every membership policy / baseline | discriminator between the two policy classes |
| `kube-vnet/system=true` | operator-created `namespace` and `cluster` system VirtualNetworks | system-vnet sentinel; checked by the system-vnet VAP |

All four are written by the operator and never set by users. They had no business under the user-surface prefix. Two practical problems followed:

1. **The system-labels VAP can't naturally protect them.** The VAP's CEL filters by key prefix `kube-vnet.system/`. The four sentinels above don't match, so users can `kubectl label networkpolicy -n traefik kube-vnet/managed-by-` to silently hide a policy from the operator's cleanup tail-step and from the chart's pre-delete hook. Drift correction recovers eventually, but the window is real.
2. **No single rule for future keys.** Every new operator-owned key landed wherever the author felt like, with no convention to enforce. Pod labels picked `.system/`; policy labels stayed bare. New contributors had no way to know.

## Decision

Every operator-managed label/annotation key uses the `kube-vnet.system/` prefix. User-managed keys use the bare `kube-vnet/` prefix. One rule, mechanically enforceable.

### Renames

| Before | After | Notes |
|---|---|---|
| `kube-vnet/managed-by=kube-vnet` | `kube-vnet.system/managed-by=kube-vnet` | On both `NetworkPolicy` and `VirtualNetwork` |
| `kube-vnet/network=<homeNS>.<vnet>` | `kube-vnet.system/network=<homeNS>.<vnet>` | Cleanup label (ADR 0010 mechanism unchanged) |
| `kube-vnet/role=membership\|baseline` | `kube-vnet.system/role=membership\|baseline` | Same values, new key |
| `kube-vnet/system=true` | **removed** | Redundant with the canonical `kube-vnet.system/managed-by=kube-vnet`. Operator-created VirtualNetworks now carry the same sentinel as operator-emitted NetworkPolicies. |

### User-facing keys (unchanged)

| Key | Owner | Stays |
|---|---|---|
| `kube-vnet/net.<vnet>=<dir>` | user (pod label) | `kube-vnet/` |
| `kube-vnet/disabled=true` | user (namespace annotation) | `kube-vnet/` |

These are inputs from users to the operator. Keeping them under `kube-vnet/` preserves the type signature: `kube-vnet/<key>` is "I'm telling the operator something", `kube-vnet.system/<key>` is "the operator is recording something".

### VAP scope extension

The `kube-vnet.system/*` system-labels `ValidatingAdmissionPolicy` (introduced in ADR 0030) was scoped to `pods`. It now also covers `networkpolicies.networking.k8s.io` and `virtualnetworks.kube-vnet.lhns.de`. The CEL expression is unchanged — any key under the `kube-vnet.system/` prefix is operator-managed; users can't add, change, or remove them on any covered resource. Operator's ServiceAccount continues to be exempted.

### System-vnet VAP

The pre-existing system-vnet VAP (checks against label-spoofing on the reserved `namespace` / `cluster` vnet names) flips its CEL from `labels["kube-vnet/system"] == "true"` to `labels["kube-vnet.system/managed-by"] == "kube-vnet"`. Same semantic, canonical key.

## Migration

Clean cutover, no dual-emit, no two-release deprecation. The reasoning:

- **On-disk policies migrate automatically via SSA.** The operator uses server-side apply with `FieldOwner("kube-vnet")` for every operator-emitted `NetworkPolicy` and `VirtualNetwork`. When the new operator starts, the first reconcile of each resource reapplies it with the new label set as the authoritative declaration. SSA strips any labels the field manager previously declared but no longer does — the old `kube-vnet/managed-by` etc. are removed without an explicit relabel step. Drift correction triggers reconcile for every managed resource on operator startup, so the sweep is fast.
- **The chart's cleanup hook ships in the same release as the operator.** The hook's selector flips to the canonical key in the same commit. They never disagree across versions.
- **User runbooks and kubectl one-liners do break.** `kubectl get netpol -A -l kube-vnet/managed-by=kube-vnet` returns nothing after upgrade. This is the same audience that absorbed the `--label-prefix` removal in the previous release; documented in CHANGELOG and the labels-and-annotations reference.

If a future release demands a softer migration, ADR 0032's two-release pattern remains available — it just isn't worth the carrying cost for this rename.

## Consequences

- Single rule for operator-owned keys: anything the operator writes lives under `kube-vnet.system/`. Future contributors don't have to ask.
- The system-labels VAP now mechanically protects operator-owned `NetworkPolicy` and `VirtualNetwork` labels from user tampering. Closing a real (small) security gap.
- One fewer sentinel on system VirtualNetworks (`kube-vnet/system=true` → gone; `kube-vnet.system/managed-by=kube-vnet` carries the same meaning, same key as NetworkPolicies).
- Cleaner mental model in docs and code.

## Alternatives considered

- **Two-release dual-emit (ADR 0032 pattern).** Operator emits both keys for one release; chart selectors match either; old keys drop in N+1. Rejected as over-engineered for this rename: SSA-on-first-reconcile already handles existing on-disk policies; the chart ships its own cleanup hook in lockstep with the operator; only user-runbooks are at risk, and they were the same audience that absorbed the `--label-prefix` removal.
- **Leave `kube-vnet/system=true` as a separate sentinel.** Rejected: it duplicates the meaning of `kube-vnet.system/managed-by=kube-vnet`; merging removes one selector users have to know.
- **Keep `kube-vnet/network` for backward-compat with ADR 0010's documented contract.** Rejected: ADR 0010 documents the *mechanism* (label-driven cross-namespace cleanup); the *key* is implementation. ADR 0010 gets a header note pointing at this ADR for the new key name.
- **Extend the user-key prefix (`kube-vnet/`) to be operator-aware via CEL key allowlists.** Rejected: complicated VAP, no clean rule.

## References

- [ADR 0010](0010-cross-namespace-cleanup-via-network-label.md) — established the cleanup label mechanism (key now renamed).
- [ADR 0030](0030-unified-vnet-membership-with-resolution.md) — introduced `kube-vnet.system/` for pod labels.
- [ADR 0032](0032-chart-crd-removal-two-release-pattern.md) — the dual-emit pattern this ADR explicitly opted out of.
