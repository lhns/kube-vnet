# 0016 — Emit events on condition transitions

Status: Accepted

## Context

Status conditions (ADR 0012) are the structured, programmatic surface for "what's the state of this VirtualNetwork?". But conditions only show up when a tool actively reads the resource. When something **changes** — e.g. `Ready` flips from True to False because a NetworkPolicy apply failed — there is no push signal unless the operator emits a Kubernetes Event.

`kubectl describe vnet payments` shows the recent Events. Event aggregators (Datadog, Grafana, etc.) consume them. Without events, an operator who is watching the cluster has to poll conditions or set up a controller of their own.

## Decision

Emit Kubernetes Events on these transitions:

| Event | Type | When |
|---|---|---|
| `Ready` | Normal | Ready transitions False → True |
| `NotReady` | Warning | Ready transitions True → False |
| `Degraded` | Warning | Degraded transitions False → True |
| `Recovered` | Normal | Degraded transitions True → False |
| `ApplyFailed` | Warning | A `NetworkPolicy` server-side apply returns an error |

The reconciler snapshots the prior `Ready` and `Degraded` condition statuses at the start of each reconcile and compares after `updateStatus` to decide whether to emit. `ApplyFailed` is emitted at the failure site, immediately, regardless of subsequent condition state.

The `Recorder` is plumbed via `mgr.GetEventRecorderFor("kube-vnet")` and lives on the reconciler struct.

## Consequences

- **Pro**: `kubectl describe vnet` shows recent state changes without forcing the user to query conditions or watch logs.
- **Pro**: Standard k8s telemetry path — every Event aggregator already understands it.
- **Pro**: Events fire only on transitions, not every reconcile; no flood at the periodic resync.
- **Con**: The reconciler now has a small amount of "before/after" plumbing (snapshot prior status, compare after). Acceptable.
- **Constraint**: Events are best-effort and have a TTL (default 1 hour) — they're a notification mechanism, not a durable audit log. Conditions remain the source of truth for current state.
