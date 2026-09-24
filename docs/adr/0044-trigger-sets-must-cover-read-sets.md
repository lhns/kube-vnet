# ADR 0044 — A reconciler's trigger set must cover its read set

**Status**: Accepted (2026-07-26)

> **Amendment (2026-09-23/24) — table rows corrected; the tables below are current.**
> - `VirtualNetworkReconciler` never read `VirtualNetworkBinding` (it reads the stamps resolution derives from bindings, which are pod events), so its binding watch was removed: four inputs, not five.
> - The auto-allow Pod watch (`ExternalAllowReconciler`, `ApiserverReachableReconciler`) fired on creates only and missed a backing pod leaving. It now enqueues only the named-`targetPort` Services whose selector the pod enters or leaves (create, delete, or a label change that flips `labelsMatchSelector`), since container ports are immutable. This narrows which changes fire, not which fields matter.
> - `VirtualNetworkBindingReconciler` now reads pod stamps (covered by its Pod watch) and its vnet's home namespace, which may differ from its own; the namespace → bindings mapper also enqueues bindings targeting a vnet homed in the changed namespace, so the "collapse into one" note below no longer holds.

## Context

Four bugs of one shape surfaced in quick succession. In each, a reconciler decided something by reading state it does not watch, so the decision was never revisited:

| Symptom | Read but not watched |
|---|---|
| Pod created before its `VirtualNetwork` → never stamped, isolated by the deny-all baseline | `VirtualNetwork` (resolution) |
| Namespace labelled to satisfy `allowedNamespaces.selector` → its pods never stamped | Namespace **labels** (resolution watched only annotations) |
| Vnet's home namespace disabled then re-enabled → vnet stays `Degraded`, policies deleted, members isolated | Namespace (`VirtualNetworkReconciler`) |
| `VirtualNetworkBinding.status.attachedPods` frozen | Pod + Namespace (`VirtualNetworkBindingReconciler`) |

The first was reported from a cluster as "rolling the pod fixed it" — and rolling was the *only* fix available.

### Why these are permanent, not merely slow

Kubernetes operators are usually forgiving here: a missed event costs you staleness until the next resync. **That safety net does not exist in this operator**, and the reason is worth stating plainly because it inverts the usual intuition.

The churn work in `75c14a6` made every predicate change-based — correctly, because membership-style predicates fired on every pod heartbeat and turned a pod restart storm into a reconcile storm. But change-based predicates compare old against new:

```go
// controller-runtime predicate.go
return !maps.Equal(e.ObjectNew.GetLabels(), e.ObjectOld.GetLabels())   // LabelChanged
return e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration()      // GenerationChanged
```

The informer resync delivers `Update` events where **old and new are the same object**. Every such predicate therefore returns false, and the resync never reaches a reconciler. `SyncPeriod` cannot save an incomplete trigger set.

Before the churn work, the membership predicates fired on unrelated pod heartbeats often enough to re-run these decisions by accident. Removing that churn was right; it also removed the accident. **Trigger-set completeness stopped being an optimisation and became a correctness requirement**, and nothing recorded that.

These were also outliers rather than edge cases: six controllers already watch `Namespace` and fire on every change to it. Exactly three deviated, and each deviation was one of the bugs above.

## Decision

**A reconciler must watch every input its decision depends on. Where the input is a *field* of a watched object, the predicate must not filter that field.**

Fan-out is scoped to the changed object's blast radius, expressed once rather than re-derived per call site. For a `VirtualNetwork` the blast radius is the namespaces it admits, because **a pod's membership can only change if its namespace may join the vnet** — which is `Permits`, inverted:

- **`NamespacesAdmittedBy(ctx, c, vnet)`** (`permits.go`) — `home ∪ allowedNamespaces`, delegating to `PermitsForVnet` so the definition lives in exactly one place.
- **`podsIn(ctx, namespaces...)`** (`resolution_controller.go`) — the one pod-enumeration, replacing four near-identical copies.

Every mapper is then a single statement of intent:

| Mapper | Body |
|---|---|
| vnet → pods | `podsIn(NamespacesAdmittedBy(vnet))` |
| namespace → vnets | vnets whose home is that ns, or that admit it |
| pod → bindings | bindings in the pod's namespace |
| namespace → bindings | bindings in that namespace, or targeting a vnet homed there (2026-09-24) |

The binding mappers collapse into one because a binding only ever selects pods **in its own namespace**, and both of its namespace-derived inputs (`IsManaged`, `nsPermits(vnet, b.Namespace)`) key on that same namespace.

Crucially this covers all four membership sources — join label, `VirtualNetworkBinding`, `VirtualNetworkBaseline`, `ClusterVirtualNetworkBaseline` — **by construction**, without matching each of them.

### Read set → trigger set

The checklist for future changes. Any read not covered by a trigger is a bug of the class above.

| Controller | Reads | Triggers |
|---|---|---|
| `VirtualNetworkReconciler` | VirtualNetwork, Pod, NetworkPolicy, **Namespace** | all four |
| `ResolutionReconciler` | Pod, Namespace (annotation **+ labels**), **VirtualNetwork**, both Baselines, Binding | all six |
| `VirtualNetworkBindingReconciler` | Binding, VirtualNetwork, **Pod** (labels incl. stamps), **Namespace** (own + vnet home, 2026-09-24) | all four |
| `NamespaceReconciler` | Namespace, NetworkPolicy | both |
| `SystemVnetReconciler` | Namespace, VirtualNetwork | both |
| `HostPortReconciler` | Namespace, Pod, NetworkPolicy | all three |
| `ExternalAllowReconciler` | Service, Namespace, Pod, NetworkPolicy | all four; Pod when it enters or leaves a named-port Service's selector (+30s requeue for pending named ports) |
| `ApiserverReachableReconciler` | Service, Namespace, Pod, NetworkPolicy, 4 discovery kinds | all; Pod when it enters or leaves a named-port Service's selector (+30s requeue) |
| `MetricsCollector` | live Lists on a 30s tick | n/a — not a reconciler |

Predicates may narrow *which* changes fire, never *which fields matter*. `GenerationChangedPredicate` on the resolution controller's `VirtualNetwork` watch is legitimate: the CRD has a status subresource, so generation tracks spec only, and without it every membership status write would fan out to pods and recreate the loop `75c14a6` removed.

## Alternatives considered

1. **Rely on the manager's `SyncPeriod` resync.** Does not work — see Context. The mechanism that would rescue us is exactly the one change-based predicates disable.

2. **A resync-aware predicate wrapper**, e.g. `Or(changed, isResync)` keyed on identical `ResourceVersion`. Tempting: one generic helper restores eventual convergence everywhere, including gaps nobody has found yet. **Rejected**, and this is the crux of the decision:
   - It reinstates the churn profile we just removed — every pod reconciled every period, which is what drove the original CPU and apiserver-traffic report.
   - It makes correctness depend on a **timer instead of causality**. Convergence time becomes the resync period rather than the propagation delay.
   - Most importantly it **hides** incomplete trigger sets. A missing watch degrades from "broken" to "slow", so the class of bug above stops being observable and starts accumulating silently. We would trade four visible bugs for an unknown number of invisible ones.

3. **Blanket `RequeueAfter` on every return path.** Same objections as (2), applied per controller, plus it interacts badly with `VirtualNetworkReconciler`, whose every reconcile runs a cluster-wide `PodList`.

4. **Per-source fan-out matching** — enumerate, for a vnet event, the pods reached via each of the four membership sources. This shipped briefly in `8be4934`. Precise, but it re-derived permission logic per source (~100 lines) and needed its own special case for refs with an omitted namespace. Superseded by the admitted-namespaces inversion, which is ~15 lines and correct for all sources at once.

## Consequences

**Positive.** Convergence is causal: events fire when an input actually changes, so steady state stays free and there is no new polling anywhere. Blast radius has one definition instead of four. The change is a net *reduction* in code — `podsIn` removed four duplicated loops and `NamespacesAdmittedBy` removed the per-source matcher.

**Negative — accepted.** Fan-out is a **superset**: a vnet event enqueues every pod in the admitted namespaces, not only those that name it. An `all: true` vnet therefore fans out cluster-wide on spec change. This is bounded — spec changes are rare, status writes are filtered, and a redundant resolution reconcile is idempotent and cheap — and the common case stays tight, since the per-namespace `namespace` system vnet admits exactly one namespace.

**Negative — accepted.** The invariant is enforced by review and tests, not by the compiler. Mitigated by the table above and by an integration test per gap, each written to fail before its fix.

**Testing.** Convergence tests must grant or restore access **without touching the object whose state must change** — that is the whole property. They must also let self-generated events drain first: a vnet whose home namespace is excluded deletes its own membership policy, and that delete re-enqueues the vnet via `policyToVNet`, so re-enabling too quickly measures the race and passes for the wrong reason. An early version of the home-namespace test did exactly that.

## References

- [ADR 0030](0030-unified-vnet-membership-with-resolution.md) — the resolution model; its 2026-07-26 amendment is one instance of this rule.
- [ADR 0034](0034-admission-webhook-for-pod-resolution.md) — *Proposed* and unimplemented when this ADR was written; stamping was purely asynchronous, so admission ordering never determined membership. Since implemented as an opt-in (2026-09-11). It stamps at pod admission only; a vnet created *after* its pods still reaches them through the watch this ADR requires.
- [ADR 0043](0043-virtualnetworkref-namespace-inferred-or-honored.md) — ref canonicalisation, which decides when an omitted namespace means "the pod's own".
