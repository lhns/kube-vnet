# 0011 — Policy naming and truncation

> **Amendment (2026-07-26) — truncate-and-hash also applies to operator-owned label *values*.**
>
> The `kube-vnet.system/source` value was built by concatenation (`"svc-" + name`, `"apiserver-" + name`) and never bounded, though label values cap at 63 characters. A Service name over 59 characters (53 behind `apiserver-`) made the server-side apply fail with `metadata.labels: Invalid value`, retried forever, so the policy never appeared. Seen with a 61-character Helm-prefixed OpenTelemetry webhook Service. `SourceLabelValue(prefix, namespace, name)` now applies this ADR's rule at 63 characters, reusing `policyHash`. Values that already fit are unchanged, so nothing is rewritten on upgrade. Every write and every List selector must go through the helper.
>
> A truncated value can't be parsed back into a Service name: the old `…PolicyToService` map functions would silently enqueue a Service that doesn't exist and stop drift correction. They were replaced by `handler.EnqueueRequestForOwner`, since every such policy has its Service as controller owner. `ExternalAllowReconciler` and `ApiserverReachableReconciler` emit policies with the same owner and role, so the `source-kind` filter moved into the watch predicates (`externalAllowPolicyPredicate`, `apiserverPolPredicate`). HostPort policies have no Service owner and are unaffected.

Status: Accepted. Refined by [ADR 0033](0033-canonical-fq-system-labels.md) and [ADR 0039](0039-uniform-kind-prefixed-policy-naming.md): the `kube-vnet-<vnet>-<ns>` format below is obsolete; names are now kind-prefixed with a hash (`kube-vnet.base`, `kube-vnet.mem.<homeNS>.<vnet>-<8hex>`, `kube-vnet.ext.{svc,host,apiserver}.…-<8hex>`). The truncate-and-hash rule survives unchanged.

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
