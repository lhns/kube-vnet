# 0011 — Policy naming and truncation

Status: Accepted (refined by [ADR 0033](0033-canonical-fq-system-labels.md) — policy names are now uniformly `kube-vnet.<homeNS>.<vnet>-<8hex>`; the bare-form `kube-vnet.<vnet>-<8hex>` and per-binding `kube-vnet.<homeNS>.<vnet>.b.<binding>-<8hex>` shapes documented here are obsolete. The truncate-and-hash logic this ADR contributes survives unchanged.)

## Context

Generated `NetworkPolicy` resources need names that are:

- **Deterministic** — the same input must produce the same name across reconciles, so SSA upserts the same object rather than churning.
- **Predictable** — operators reading `kubectl get networkpolicy` should be able to tell at a glance which VirtualNetwork a policy belongs to.
- **Within Kubernetes' 253-character resource-name limit** — VirtualNetwork name + namespace name can theoretically exceed this.

## Decision

Format: `kube-vnet-<vnetName>-<namespaceName>`.

If the resulting name exceeds 253 characters, truncate the front and append a 4-byte sha256 hash suffix of the full untruncated name:

```go
sum := sha256.Sum256([]byte(fullName))
suffix := "-" + hex.EncodeToString(sum[:4])  // e.g. "-1a2b3c4d"
truncated := fullName[:253-len(suffix)] + suffix
```

The hash makes the truncated form unique even if two long names share a prefix.

The `kube-vnet.system/managed-by=kube-vnet` and `kube-vnet.system/network=<home>.<vnet>` labels remain the **actual source of truth** for ownership lookups (per ADR 0010). The name is for human readability; the label is for the operator.

## Consequences

- **Pro**: Most policies have human-readable names like `kube-vnet-payments-platform`.
- **Pro**: Truncation is deterministic, so SSA stays idempotent even at the limit.
- **Pro**: The operator never relies on the name for correctness; ownership lookups use labels.
- **Con**: Truncated names lose readability. Acceptable: this only happens for very long namespace+vnet name pairs; the labels still identify the owner.
