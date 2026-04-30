# 0009 — Server-side apply with field manager

Status: Accepted

## Context

The operator must (a) reliably reconcile its own `NetworkPolicy` resources to match desired state, (b) coexist with user-managed policies in the same namespace, and (c) revert drift on its own resources without stomping on fields the user has touched intentionally.

Two paths:

1. Read-modify-write with optimistic concurrency. Verbose; fragile under conflicts; awkward for "create or update".
2. Server-side apply (SSA) with a stable field manager name. Patch-based; the apiserver handles ownership and conflicts; "create or update" is one call.

## Decision

Use **server-side apply** for every operator-managed `NetworkPolicy`:

```go
r.Patch(ctx, p, client.Apply, client.FieldOwner("kube-vnet"), client.ForceOwnership)
```

The field manager `kube-vnet` is stable across operator restarts. `ForceOwnership` ensures we reclaim fields from any other manager that may have written them — the contract is that the operator owns its policies' content end-to-end.

## Consequences

- **Pro**: Drift correction is automatic. A user editing `kube-vnet-payments-platform` to add an allow rule will lose it on the next reconcile, which is the correct behavior — operator-managed policies are the operator's source of truth.
- **Pro**: Coexistence with user-managed `NetworkPolicy` resources is unaffected: SSA only manages fields the operator sets on its own objects. NetworkPolicies are ORed by Kubernetes, so the user's separate resources continue to work.
- **Pro**: One code path for create + update; "patch instead of read-modify-write" reduces conflicts.
- **Con**: SSA is more nuanced than read-modify-write — managed-fields metadata grows on the resource. Standard cost of the pattern.
