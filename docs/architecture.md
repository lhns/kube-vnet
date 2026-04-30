# Architecture

How kube-vnet is built. This document describes the runtime structure of the operator: which components do what, how they interact, where to look in the source.

For *why* each piece is the way it is, the [ADRs](adr/README.md) are the source of truth. This document describes the *what*, with cross-references to the relevant ADR for the *why*.

---

## High level

```
┌─ apiserver ──────────────────────────────────────────────────────────┐
│                                                                      │
│   VirtualNetwork (CRD)        Namespace        Pod          NetworkPolicy │
│        │                         │              │                 ▲  │
│        │ watches                 │ watches      │ watches         │  │
│        ▼                         ▼              ▼                 │  │
│   ┌──────────────────────────────────────────────────────────┐    │  │
│   │     controller-runtime Manager (kube-vnet-controller)    │    │  │
│   │                                                          │    │  │
│   │   ┌───────────────────────────┐  ┌────────────────────┐  │    │  │
│   │   │ VirtualNetworkReconciler  │  │ NamespaceReconciler│  │    │  │
│   │   │  (per-vnet)               │  │  (flag-driven)     │  │    │  │
│   │   └────────────┬──────────────┘  └────────┬───────────┘  │    │  │
│   │                │ uses                      │ uses          │    │  │
│   │                ▼                           ▼               │    │  │
│   │     ┌─────────────────┐       ┌────────────────────┐       │    │  │
│   │     │ policy_generator│       │      baseline      │       │    │  │
│   │     │   (pure func)   │       │   (DesiredBaseline)│       │    │  │
│   │     └────────┬────────┘       └────────────┬───────┘       │    │  │
│   │              │                              │               │    │  │
│   │              └──────────────┬───────────────┘               │    │  │
│   │                             │ server-side apply             │    │  │
│   │                             └───────────────────────────────┼────┘  │
│   │                                                             │       │
│   │   ┌─────────────────────────────┐                           │       │
│   │   │ MetricsCollector (30s tick) │  reads vnets + policies   │       │
│   │   └─────────────────────────────┘  (sets gauges)            │       │
│   └─────────────────────────────────────────────────────────────┘       │
└──────────────────────────────────────────────────────────────────────────┘
```

The operator is a single process (`cmd/main.go`) running a controller-runtime `Manager`. The Manager hosts:

- **`VirtualNetworkReconciler`** — primary reconciler; watches `VirtualNetwork`, `Pod`, and `NetworkPolicy`. Owns membership-driven baselines.
- **`NamespaceReconciler`** — watches `Namespace`. Owns the *flag-driven* baseline lifecycle (`--default-deny-everywhere`).
- **`MetricsCollector`** — a `Runnable` that ticks every 30 seconds and updates cluster-wide gauges (`kube_vnet_networks_total`, `kube_vnet_managed_policies_total`).

The reconciler delegates two pure-functional pieces:

- **`policy_generator.go:Generate`** — takes a VirtualNetwork and a member set, returns the desired `[]NetworkPolicy`. No client, no I/O. ([ADR 0008](adr/0008-pure-function-policy-generator.md))
- **`baseline.go:DesiredBaseline`** — returns the `kube-vnet-default-deny` policy spec for a given namespace. Pure.

Plus a small predicate component:

- **`namespace.go:NamespaceFilter`** — `IsManaged(ns)` answers "should the operator act in this namespace?". Combines the operator-level `--excluded-namespaces` flag and the per-namespace `kube-vnet/disabled=true` annotation.

---

## The reconciliation loop (`VirtualNetworkReconciler`)

Source: `internal/controller/virtualnetwork_controller.go`.

For each enqueued `VirtualNetwork` request, `Reconcile` does roughly:

1. **Metrics setup**. `start := time.Now()` — `defer observeReconcile(start, err)` records duration and outcome.
2. **Fetch** the VirtualNetwork. If 404, run `cleanupForDeleted` (removes every `NetworkPolicy` carrying this vnet's `kube-vnet/network` label, including across namespaces) and return.
3. **Snapshot** prior `Ready` and `Degraded` condition statuses so we can emit transition events later.
4. **Validate** the name against `nameRegex` (DNS-1123 label). If it fails, set `Ready=False`/`Degraded=True` with reason `InvalidName`, write status, return. (Defense-in-depth — the CRD's CEL rule should already have rejected this at admission. See [ADR 0017](adr/0017-name-validation-via-cel-and-runtime-check.md).)
5. **Check the home namespace** via `r.NSFilter.IsManaged(homeNS)`. If unmanaged, set `Ready=False`/`Degraded=True` reason `HomeNamespaceExcluded`, write status, run `cleanupForDeleted` to drop any leftover policies, return.
6. **Discover members** (`discoverMembers`):
   - List pods cluster-wide.
   - For each pod that carries the appropriate join-label key for its namespace (bare for home, prefixed for foreign):
     - If the pod's namespace is unmanaged → record as `InvalidJoiner` with reason `NamespaceExcluded`.
     - If the pod's namespace is the home namespace → it's a member.
     - Otherwise consult `permits(vnet, pod.Namespace)`, which evaluates `spec.allowedNamespaces` (`All` / `Names` / `Selector`). If permitted → member. If not → `InvalidJoiner` with reason `NamespaceNotAllowed`.
7. **Generate** the desired `NetworkPolicy` set via `policy_generator.Generate`.
8. **Apply** each desired policy via `applyPolicyAndDetectRestore`:
   - Uncached `Get` (uses `APIReader`) to detect a "policy was just deleted" state → emit a `PolicyRestored` event.
   - Server-side apply with `client.FieldOwner("kube-vnet")` and `client.ForceOwnership`.
   - On failure: increment `kube_vnet_apply_errors_total{kind="membership_policy"}`, emit `ApplyFailed` event, set `Ready=False` reason `ApplyFailed`, return.
9. **Ensure baseline** in every managed namespace that has members. Same `applyPolicyAndDetectRestore` path; failure increments `apply_errors_total{kind="baseline"}`.
10. **Delete stale** policies (`deleteStale`): list policies with the `kube-vnet/network` label, drop any not in the desired set, then `gcBaselineIfEmpty` for each emptied namespace.
11. **Compute conditions**:
    - `Degraded` = True / `InvalidJoiners` if any pods were rejected; False / `NoIssues` otherwise.
    - `Ready` = True / `NoMembers` if no policies were generated; True / `PoliciesGenerated` otherwise.
12. **Write status** via the `/status` subresource (`Members`, `GeneratedPolicies`, `ObservedGeneration`).
13. **Emit events** on Ready/Degraded transitions (`emitTransitionEvents`).
14. **Update per-vnet gauge**: `kube_vnet_members_total{network="<homeNS>/<name>"}` = total member count.
15. **Return** with `RequeueAfter: 10 * time.Minute` (safety-net resync).

### Cleanup paths

- `cleanupForDeleted(ns, name)` — invoked when the VirtualNetwork is gone. Lists policies cluster-wide by `kube-vnet/managed-by=kube-vnet, kube-vnet/network=<ns>.<name>`, deletes them, then runs `gcBaselineIfEmpty` on each namespace that just had a policy removed.
- `deleteStale(vnet, desiredKeys)` — invoked during normal reconcile. Same shape but only deletes policies *not* in the new desired set.
- `gcBaselineIfEmpty(ns)` — uses the uncached `APIReader` (avoids informer staleness) to list `kube-vnet/managed-by=kube-vnet, kube-vnet/role=membership` policies in `ns`. If zero, deletes the baseline. See [ADR 0019](adr/0019-baseline-durability.md) on the cache-staleness concern.

---

## The two reconcilers

There are deliberately two reconcilers, both writing the same `kube-vnet-default-deny` baseline shape. Server-side apply with the same field manager makes their writes idempotent.

| Owns | When it acts | Trigger |
|---|---|---|
| `VirtualNetworkReconciler` | Membership-driven baseline (and all per-vnet membership policies) | Per-vnet reconcile when at least one pod joins |
| `NamespaceReconciler` | Flag-driven baseline (`--default-deny-everywhere=true`) | Per-namespace reconcile, only when the flag is on |

When the flag is off, `NamespaceReconciler` short-circuits — every event is a no-op. When on, it ensures the baseline in every managed namespace, even those with no members. See [ADR 0020](adr/0020-default-deny-unmanaged-namespaces.md).

The split keeps the per-vnet reconciler simple (it only thinks about its own vnet) and makes the flag a localized addition rather than a fork in the main reconcile path.

---

## Watches and predicates

The `VirtualNetworkReconciler.SetupWithManager` wires three watches:

| Watch | Predicate | Mapper |
|---|---|---|
| `VirtualNetwork` | none (primary) | identity |
| `Pod` | label-prefix predicate (only events whose pod's labels contain at least one `kube-vnet/net.*` key, on either old or new state) | `handler.Funcs` mapping each `kube-vnet/net.*` label key to the corresponding VirtualNetwork |
| `NetworkPolicy` | managed-by predicate (`kube-vnet/managed-by=kube-vnet`) | `policyToVNet` mapping the `kube-vnet/network` label to the owning VirtualNetwork |

The Pod watch uses **`handler.Funcs`** instead of `handler.EnqueueRequestsFromMapFunc` because removals matter: if a pod loses its join label, the *current* labels don't reveal which vnet it just left, but the *old* labels do. `handler.Funcs.UpdateFunc` sees both `e.ObjectOld` and `e.ObjectNew` and enqueues the union of vnets referenced by either side. Without this, a pod losing its label would leave the previous vnet's status stale until the 10-minute resync. See [ADR 0013](adr/0013-pod-watch-with-handler-funcs-for-removals.md).

The NetworkPolicy watch is what gives us drift correction: a delete or a hand-edit fires an event, the policy's `kube-vnet/network` label maps it back to a VirtualNetwork, and the reconciler re-applies. See [ADR 0019](adr/0019-baseline-durability.md).

The `NamespaceReconciler` watches `Namespace` only; no predicate (every Namespace event is interesting when the flag is on; the reconciler short-circuits when off).

The Manager's `SyncPeriod` defaults to 10 minutes — every controller resyncs all primary objects after that interval. For us, `Reconcile` returns `RequeueAfter: 10 * time.Minute` explicitly to keep the cadence predictable.

---

## The pure-function policy generator

`internal/controller/policy_generator.go:Generate` takes:

```go
type GenerateInput struct {
    VNet        *vnetv1alpha1.VirtualNetwork
    LabelPrefix string
    MembersByNS map[string][]string  // namespace -> pod names (informational; selectors match by label)
}
```

and returns:

```go
type GenerateOutput struct {
    Policies []networkingv1.NetworkPolicy
}
```

No client, no context, no I/O. The reconciler is responsible for I/O; the generator is responsible for "given this state, what does the desired NetworkPolicy set look like?"

For each namespace with members:

1. Build a peer rule for every member-bearing namespace: `{namespaceSelector: kubernetes.io/metadata.name=<peerNS>, podSelector: Exists <join-key-for-peerNS>}`. The peer's join-key form is bare if peerNS == homeNS, prefixed otherwise.
2. Build the policy: name `kube-vnet-<vnet>-<ns>` (truncate-and-hash if > 253 chars per [ADR 0011](adr/0011-policy-naming-and-truncation.md)), labels `kube-vnet/managed-by=kube-vnet, kube-vnet/network=<homeNS>.<vnet>, kube-vnet/role=membership`, the namespace's join-key form as the `podSelector`, all peer rules in `ingress[0].from` and `egress[0].to`, plus the DNS allow rule in egress.
3. If the policy is in the home namespace, attach an `OwnerReference` to the VirtualNetwork. Cross-namespace policies have no owner ref ([ADR 0010](adr/0010-cross-namespace-cleanup-via-network-label.md)).

The generator is exhaustively unit-tested — see `policy_generator_test.go`. Trivial to extend (a new policy field becomes a new test case + a new line in `Generate`).

---

## Server-side apply with field manager

Every operator-managed `NetworkPolicy` is written via:

```go
r.Patch(ctx, p, client.Apply,
    client.FieldOwner("kube-vnet"),
    client.ForceOwnership,
)
```

`client.Apply` is server-side apply (SSA). `FieldOwner` is the stable identity used by SSA to track who owns which fields. `ForceOwnership` reclaims fields if some other actor wrote them.

Net effect:

- Drift correction is automatic. A user editing `kube-vnet-payments-platform.spec` to add an allow rule will lose it on the next reconcile; the operator owns its policies' content end-to-end.
- Coexistence with user-managed `NetworkPolicy` (different objects, same namespace) is unaffected — SSA only governs fields on the operator's own objects. NetworkPolicies are ORed by Kubernetes; user policies compose additively.
- `Create or Update` is one call. No optimistic-concurrency loop in the reconciler.

Details: [ADR 0009](adr/0009-server-side-apply-with-field-manager.md).

---

## Cross-namespace cleanup via the network label

Kubernetes does **not** support cross-namespace owner references. A namespaced VirtualNetwork in `monitoring` cannot own a `NetworkPolicy` in `platform` via owner refs.

The operator solves this by labeling every operator-managed policy with `kube-vnet/network=<homeNS>.<vnet>`. On VirtualNetwork deletion, `cleanupForDeleted` lists policies cluster-wide by this label and deletes them.

The home-namespace policy *does* have a normal `OwnerReference` to the VirtualNetwork — cleanup of that one is also handled by Kubernetes garbage collection. The label-based cleanup is the cross-namespace fallback. See [ADR 0010](adr/0010-cross-namespace-cleanup-via-network-label.md).

---

## Baseline lifecycle

The `kube-vnet-default-deny` baseline appears and disappears based on need:

- **Appears** when at least one pod in a managed namespace becomes a member of any VirtualNetwork (membership-driven, owned by `VirtualNetworkReconciler`).
- **Appears** in every managed namespace if `--default-deny-everywhere` is on (flag-driven, owned by `NamespaceReconciler`).
- **Survives** as long as either of the two conditions above holds. Deleting one vnet that had members doesn't remove the baseline if another vnet's members are still in the namespace.
- **Removed** by `gcBaselineIfEmpty` when the namespace has zero `kube-vnet/role=membership` policies left *and* the flag-driven path doesn't want it.

`gcBaselineIfEmpty` uses the **uncached `APIReader`** for its policy-count check. A normal cached `List` could see a just-deleted membership policy as still present (informer cache lag), causing the GC to skip and orphan the baseline. The uncached read closes that race. See [ADR 0019](adr/0019-baseline-durability.md).

---

## Drift correction loop

```
   ┌──────────────────────────────────────────────┐
   │ 1. operator applies desired state            │
   │    (membership policy or baseline)           │
   └─────────────────────┬────────────────────────┘
                         │
                         ▼
   ┌──────────────────────────────────────────────┐
   │ 2. someone (user, tool) deletes / edits      │
   │    the operator-managed NetworkPolicy        │
   └─────────────────────┬────────────────────────┘
                         │ apiserver fires watch event
                         ▼
   ┌──────────────────────────────────────────────┐
   │ 3. NetworkPolicy watch predicate matches     │
   │    (kube-vnet/managed-by=kube-vnet)          │
   │    → policyToVNet maps to owning VirtualNetwork │
   │    → enqueue                                  │
   └─────────────────────┬────────────────────────┘
                         │
                         ▼
   ┌──────────────────────────────────────────────┐
   │ 4. Reconcile re-runs                         │
   │    applyPolicyAndDetectRestore notices the   │
   │    pre-apply absence and emits PolicyRestored │
   │    Warning event on the owning VirtualNetwork │
   └──────────────────────────────────────────────┘
```

Window between deletion and restore: sub-second to a few seconds in practice. During the window, traffic that the policy would have denied is allowed.

This is a best-effort defense, not a hard guarantee. Hard isolation against namespace owners with NetworkPolicy-delete RBAC requires `AdminNetworkPolicy` (cluster-scoped, separate RBAC, higher precedence). Tracked as the future direction in [ADR 0019](adr/0019-baseline-durability.md).

---

## Metrics collector

`MetricsCollector` is a `manager.Runnable` that ticks every 30 seconds:

- Lists `VirtualNetwork` resources cluster-wide → sets `kube_vnet_networks_total`.
- Lists `NetworkPolicy` cluster-wide filtered by `kube-vnet/managed-by=kube-vnet` → sets `kube_vnet_managed_policies_total`.

Per-reconcile metrics (`kube_vnet_reconciliations_total`, `kube_vnet_reconcile_duration_seconds`, `kube_vnet_members_total{network}`, `kube_vnet_apply_errors_total{kind}`) are updated by the reconciler at the right moment in its loop. See [`reference/metrics-and-events.md`](reference/metrics-and-events.md) for the full list.

The cluster-wide gauges are off the hot reconcile path because they're cluster-scoped properties. Updating them per-reconcile would bias toward whichever vnet was just reconciled.

---

## Code map

```
api/v1alpha1/
  virtualnetwork_types.go       # CRD Go types + kubebuilder markers
  groupversion_info.go          # scheme registration
  zz_generated.deepcopy.go      # generated by controller-gen

cmd/
  main.go                       # flag parsing, manager setup, reconciler registration

internal/controller/
  virtualnetwork_controller.go  # the primary reconciler
  namespace_reconciler.go       # the flag-driven namespace reconciler
  policy_generator.go           # pure-function NetworkPolicy generator
  baseline.go                   # DesiredBaseline + baseline constants
  namespace.go                  # NamespaceFilter (IsManaged)
  metrics.go                    # Prometheus metrics + MetricsCollector
  *_test.go                     # unit tests
  suite_integration_test.go     # envtest TestMain (build tag integration)
  integration_test.go           # envtest tests (build tag integration)

config/                         # kustomize bases (CRD, RBAC, manager Deployment)
charts/kube-vnet/               # Helm chart
test/e2e/                       # kind+CNI e2e tests + bootstrap scripts
docs/                           # this documentation
```

---

## Where to read more

- For the full set of accepted decisions: [`adr/`](adr/README.md).
- For the long-form rationale: [`kube-vnet-design.md`](kube-vnet-design.md).
- For *what* each label/annotation/metric/event/condition reason means: [`reference/`](reference/).
