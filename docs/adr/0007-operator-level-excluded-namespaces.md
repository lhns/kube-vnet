# 0007 — Operator-level excluded namespaces

Status: Accepted

## Context

Some namespaces should never be touched by the operator regardless of their annotations or pod labels — most notably `kube-system` (control plane), and the operator's own namespace (so the operator can't accidentally lock itself out by misconfiguration). Requiring the cluster operator to remember to annotate these correctly on every install is fragile.

## Decision

Add a controller flag `--excluded-namespaces` (comma-separated) with sensible defaults:

```
--excluded-namespaces=kube-system,kube-public,kube-node-lease
```

The operator's own namespace is always added at startup, derived from the `POD_NAMESPACE` environment variable (set via the downward API in the Deployment).

Excluded namespaces have **identical semantics** to per-namespace `kube-vnet/disabled: "true"` (ADR 0006): no baseline, no membership policies, no eligibility as a joiner.

## Consequences

- **Pro**: A fresh install is safe by default — the control plane is never touched.
- **Pro**: One predicate (`NamespaceFilter.IsManaged`) covers both the flag and the annotation; the rest of the reconciler doesn't have to care which one excluded a namespace.
- **Pro**: Cluster operators can extend exclusion (e.g. for legacy namespaces being migrated) without editing every namespace's annotations.
- **Con**: A flag is mutable; changing it requires a controller restart. Acceptable for an exclusion list; if a richer policy is needed, a CRD can supersede this.
