# 0013 — Pod watch with handler.Funcs for removals

Status: Accepted

## Context

The operator must reconcile a VirtualNetwork when any pod's membership changes — both **adds** (a new pod gains the join label, or a pod is created with it) and **removes** (a pod loses the join label, or is deleted).

Adds are easy: the pod's current labels reveal which VirtualNetwork it joined. The natural shape is `handler.EnqueueRequestsFromMapFunc(podToVNets)` where `podToVNets` reads `obj.GetLabels()` and enqueues the matching VirtualNetwork.

Removes are the hard case: when a pod's label is **removed**, `obj.GetLabels()` returns the *new* state (no label), so the mapper enqueues nothing. The VirtualNetwork the pod just left has no idea it lost a member until the next periodic resync (10 minutes).

The design doc proposed an in-memory cache `pod UID → []VirtualNetwork` updated on each reconcile. That works but adds stateful complexity, and the cache must survive controller restarts (or accept stale memberships through the restart window).

## Decision

Use **`handler.Funcs`** instead of `EnqueueRequestsFromMapFunc`. `UpdateFunc` receives both `e.ObjectOld` and `e.ObjectNew`, so the mapper can extract every `kube-vnet/net.*` key from **both** objects' labels and enqueue the union.

```go
handler.Funcs{
    CreateFunc:  enqueueFromLabels(e.Object),
    UpdateFunc:  enqueueFromLabels(e.ObjectOld) ∪ enqueueFromLabels(e.ObjectNew),
    DeleteFunc:  enqueueFromLabels(e.Object),
    GenericFunc: enqueueFromLabels(e.Object),
}
```

The label-prefix predicate must also fire when *either* old or new has a `kube-vnet/net.*` label, otherwise removal events would be filtered out before reaching the handler.

## Consequences

- **Pro**: No stateful cache, no restart concerns, no concurrency issues.
- **Pro**: Removes propagate immediately on the next pod event, not only at the periodic resync.
- **Pro**: A pod relabeled across vnets (`net.payments` → `net.monitoring`) enqueues both — the leaving and the joining — in one event.
- **Con**: `handler.Funcs` is slightly more verbose than `EnqueueRequestsFromMapFunc`. Acceptable.
- **Note**: This is the only correctness fix added during the v1 hardening pass. The design doc proposed a stateful cache; this ADR records the simpler alternative.
