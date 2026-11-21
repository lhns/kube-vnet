# ADR 0046 — Rewrite stamps on pod create

**Status**: Accepted (2026-11-21)

Amends: [ADR 0034](0034-admission-webhook-for-pod-resolution.md) (stamp ownership on CREATE).

## Context

Velero restores a pod with the labels it was backed up with, including the operator-owned `kube-vnet.system/*` stamps. Restored into another namespace, the stamps name the old namespace and no longer match resolution. ADR 0034 had the mutator leave request-supplied stamps alone so the validating webhook (`failurePolicy: Fail`) denied forged ones with a message. For a restore that means the pod is denied, and a bare pod is lost. The same happens with `kubectl get pod -o yaml | kubectl apply -f -` and with manifests copied from a live pod.

## Decision

On pod CREATE the mutator owns every stamp: it recomputes the resolved set, removes or rewrites what the request carried, and adds an admission warning naming the replaced keys. On UPDATE nothing changes: only untouched stamps are the mutator's, and the validator denies a changed stamp that does not match resolution. The validator is unchanged.

## Consequences

- A forged stamp on create is replaced and the client is warned, not denied. It grants nothing: the stored stamps are always the resolved ones.
- If the mutator is unreachable (`failurePolicy: Ignore`), the validator still sees the stale stamps and denies. It fails closed.
- With the webhook off, the `system-labels-vap` still rejects any `kube-vnet.system/*` label on pod CREATE by a non-operator user, so a restore must strip them; see [Backup and restore](../guides/operations.md#backup-and-restore-velero).
- UPDATE keeps its stricter behavior, so a forged stamp on a running pod still gets the validator's message.
