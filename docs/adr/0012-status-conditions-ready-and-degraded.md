# 0012 — Status conditions: Ready and Degraded

Status: Accepted

## Context

Users and tooling need a structured signal telling them whether the operator has successfully reconciled a `VirtualNetwork`. Without one, `kubectl get vnet` can't show a meaningful `READY` column, `kubectl wait --for=condition=Ready vnet/foo` doesn't work, and dashboards have no surface to consume.

### What status conditions are (for readers new to the pattern)

A **status condition** is a typed boolean observation Kubernetes uses everywhere — `Pod.Ready`, `Node.Ready`, `Deployment.Available`, etc. Each entry in `status.conditions[]` carries:

- `type` — what is being observed (e.g. `Ready`, `Degraded`).
- `status` — `True`, `False`, or `Unknown`.
- `reason` — short, machine-readable, stable across reconciles for the same situation (e.g. `PoliciesGenerated`, `ApplyFailed`).
- `message` — human-readable detail.
- `lastTransitionTime` — when this condition last changed `status`.

The `metav1.Condition` type and its surrounding helpers (`SetStatusCondition`, etc.) are the standard k8s implementation. Tooling like `kubectl` and external automation rely on this shape.

## Decision

The operator maintains two conditions on every `VirtualNetwork`:

### `Ready`

| Status | Reason | Meaning |
|---|---|---|
| True | `PoliciesGenerated` | The desired NetworkPolicy set has been applied. |
| True | `NoMembers` | Reconciled successfully; no pods are joining this VirtualNetwork yet. |
| False | `ApplyFailed` | An apply call failed; see message for the apiserver error. |
| False | `InvalidName` | The VirtualNetwork name violates the DNS-1123 label regex (defense-in-depth — the CRD CEL rule should prevent this from being persisted). |
| False | `HomeNamespaceExcluded` | The home namespace is in `--excluded-namespaces` or has `kube-vnet/disabled=true`. |

### `Degraded`

| Status | Reason | Meaning |
|---|---|---|
| True | `InvalidJoiners` | Some pods carry the prefixed join label but live in a non-permitted namespace; their join is ignored. The VirtualNetwork is still functional for valid joiners. |
| True | `InvalidName` / `HomeNamespaceExcluded` | Mirrors the same condition under Ready=False; lets dashboards alert on `Degraded=True` regardless of cause. |
| False | `NoIssues` | Reconciled cleanly, no issues observed. |

`Reconciling` (the design doc's optional third condition) is not implemented. Adds noise; the standard pattern is to surface in-flight state via metrics or the `lastTransitionTime` deltas.

The `Ready` condition value is exposed as a printer column on the CRD so `kubectl get vnet` shows it.

Transitions emit events (per ADR 0016) so `kubectl describe vnet` and event aggregators see what changed without needing to diff conditions.

## Consequences

- **Pro**: Standard k8s pattern; `kubectl wait --for=condition=Ready vnet/foo` works out of the box.
- **Pro**: Reasons are stable strings — automation can match on `reason=ApplyFailed` without parsing free-text messages.
- **Pro**: Limited to two conditions; less ambiguity than five overlapping ones.
- **Con**: A reader has to learn the reason set. Listed in this ADR as the canonical reference.
