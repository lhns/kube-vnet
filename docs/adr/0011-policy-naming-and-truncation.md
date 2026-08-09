# 0011 — Policy naming and truncation

> **Amendment (2026-07-26) — truncate-and-hash applies to operator-owned label *values*, not only to names.**
>
> This ADR bounded policy **names** (253 chars). The `kube-vnet.system/source` label **value** was built by plain concatenation — `"svc-" + svc.Name`, `"apiserver-" + svc.Name` — and never bounded, even though Kubernetes caps label values at **63** characters, a much tighter budget than the name limit this ADR was written against. A Service name over 59 characters (or 53, behind the longer `apiserver-` prefix) produced an invalid label, the apiserver rejected the server-side apply as `metadata.labels: Invalid value`, and the reconciler retried it with capped backoff **forever** — so the policy never appeared at all. Seen in the field with a Helm-prefixed OpenTelemetry operator webhook Service (61 chars).
>
> `SourceLabelValue(prefix, namespace, name)` now applies this ADR's rule at the 63-char limit, reusing `policyHash` for the disambiguating suffix. Values that already fit are returned unchanged, so existing policies keep their labels and nothing is rewritten on upgrade.
>
> **The value is no longer reversible, and that changed a control-flow assumption.** It had three uses: written as metadata, used as a List selector to delete a Service's policies, and *parsed back* into a Service name to enqueue reconciles. Truncation is fine for the first two — as long as write and query both go through the helper, which is why every call site must — but it breaks the third silently: a truncated name enqueues a Service that does not exist, killing drift correction with no error. The two `…PolicyToService` map functions were therefore replaced by `handler.EnqueueRequestForOwner`, which every generated policy already supports (`SetControllerReference(svc, …)`) and which is immune to label-format changes. ADR 0038's own code comment already preferred owner-refs here, "because the [LabelSource] format has changed twice".
>
> One consequence worth noting: the `ExternalAllowReconciler` and `ApiserverReachableReconciler` emit policies with the **same owner and role**, differing only by `source-kind`. The deleted map function filtered on that; with an owner-ref handler the filtering has to live in the watch predicate instead, so `externalAllowPolicyPredicate` now checks `source-kind` exactly as `apiserverPolPredicate` always did. HostPort policies need no such exclusion — they carry no Service owner, so an owner-ref handler never resolves them at all.

Status: Accepted (refined by [ADR 0033](0033-canonical-fq-system-labels.md) — policy names are uniformly `kube-vnet.<homeNS>.<vnet>-<8hex>`; the bare-form `kube-vnet.<vnet>-<8hex>` and per-binding `kube-vnet.<homeNS>.<vnet>.b.<binding>-<8hex>` shapes documented here are obsolete. The truncate-and-hash logic this ADR contributes survives unchanged. **Further amended by [ADR 0039](0039-uniform-kind-prefixed-policy-naming.md): the shape gains an explicit kind segment — `kube-vnet.mem.<homeNS>.<vnet>-<8hex>` for membership, `kube-vnet.base` for baseline, `kube-vnet.ext.svc.<svcName>-<8hex>` for external-allow, `kube-vnet.ext.host.<port>.<proto>-<8hex>` for hostPort (ADR 0040).**)

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
