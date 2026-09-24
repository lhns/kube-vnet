# Architecture

The runtime structure of the operator: which components do what, how they interact, where to look in the source. For *why* each piece is the way it is, the [ADRs](../adr/README.md) are the source of truth; the file-by-file map is [`code-structure.md`](code-structure.md).

---

## High level

```
 apiserver ──watch──► controller-runtime Manager (cmd/main.go)
    ▲                   │
    │                   ├─ ResolutionReconciler ──────► pod labels kube-vnet.system/net.*
    │                   ├─ VirtualNetworkReconciler ──► membership NetworkPolicies + vnet status
    │                   ├─ NamespaceReconciler ───────► kube-vnet.base baseline
    │                   ├─ SystemVnetReconciler ──────► `namespace` / `cluster` VirtualNetworks
    │                   ├─ VirtualNetworkBindingReconciler ► binding status
    │                   ├─ ExternalAllow / HostPort / ApiserverReachable ► kube-vnet.ext.* policies
    │                   ├─ MetricsCollector (30s tick)
    └──admission────────┴─ pod-resolution webhooks (optional, --webhook-enabled)
```

The operator is a single process (`cmd/main.go`) running a controller-runtime `Manager`. The Manager hosts:

- **`ResolutionReconciler`** — computes each pod's effective `(vnet, direction)` set via the three-tier lattice (cluster baseline → namespace baseline → pod tier; intersection on within-tier conflicts, [ADR 0031](../adr/0031-baseline-tier-resolution.md)) and patches the `kube-vnet.system/net.*` and `kube-vnet.system/host-port.*` labels plus the `resolved-generation`/`resolved-by` annotations onto the pod. Emits `VirtualNetworkNotJoinable`, `InvalidJoinLabelDirection`, `ResolutionConflict` and `OverrideRejected` Events. The resolution logic lives in the shared `Resolver` (`resolver.go`) and the stamp helpers (`stamps.go`), which the admission webhook also uses.
- **`VirtualNetworkReconciler`** — generates each vnet's membership policies from the stamped pods and maintains vnet status. Does **not** touch the baseline.
- **`NamespaceReconciler`** — **sole owner** of the baseline: applies the uniform deny-all `kube-vnet.base` to every managed namespace and deletes it from disabled ones. See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) and [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md).
- **`SystemVnetReconciler`** — ensures the `namespace` system vnet exists in every managed namespace (and is deleted from disabled ones) and the `cluster` system vnet exists in the operator's namespace; drift-corrects deletes.
- **`VirtualNetworkBindingReconciler`** — maintains each binding's status (`Ready`, `attachedPods`). Binding-selected pods are stamped by the `ResolutionReconciler` like any other member.
- **`ExternalAllowReconciler`** — emits `kube-vnet.ext.svc.*` allows for externally exposed Services ([ADR 0038](../adr/0038-auto-allow-externally-exposed-services.md)).
- **`HostPortReconciler`** — emits `kube-vnet.ext.host.*` allows per `(namespace, port, protocol)` ([ADR 0040](../adr/0040-auto-allow-hostport-pods.md)).
- **`ApiserverReachableReconciler`** — emits `kube-vnet.ext.apiserver.*` allows for Services the apiserver dials, discovered from webhook configurations, `APIService`s and CRD conversion webhooks ([ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md)).
- **`MetricsCollector`** — a `Runnable` that updates the cluster-wide gauges every 30 seconds.
- **Pod-resolution webhooks** (`internal/webhook/podresolution`, only with `--webhook-enabled`) — the `Mutator` (`/mutate-v1-pod`) stamps the pod during admission using the same `Resolver`, so the pod is a member from creation; the `Validator` (`/validate-v1-pod`) rejects requests whose `kube-vnet.system/*` label changes disagree with resolution, exempting the operator's own ServiceAccount. See [ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md).
- **Network wait** (`internal/networkwait`, only with `--network-wait-beacons`) — on pod CREATE the `Mutator` also prepends an init container to pods annotated `kube-vnet/network-max-wait`. It is the operator binary's `network-wait` subcommand, which dials the chart's per-node beacons (the same binary's `network-beacon` subcommand) until every node's CNI admits the pod, or the pod's maximum runs out. See [ADR 0045](../adr/0045-network-wait-for-opted-in-pods.md).

Pure pieces the reconcilers delegate to:

- **`policy_generator.go:Generate`** — VirtualNetwork + member set in, desired `[]NetworkPolicy` out. No client, no I/O ([ADR 0008](../adr/0008-pure-function-policy-generator.md)).
- **`baseline.go:DesiredBaseline`** — the deny-all baseline for a namespace: `podSelector: {}`, `policyTypes: [Ingress]`, no rules.
- **`resolution.go:Resolve`** — ordered `ResolutionLayer`s (cluster baseline, namespace baseline, pod tier) in, effective `(VnetKey, Direction)` set out.
- **`namespace.go:NamespaceFilter`** — `IsManaged(ns)`: combines `--disabled-namespaces` (plus the operator's own namespace) with the `kube-vnet/disabled=true` annotation.

---

## The reconciliation loop (`VirtualNetworkReconciler`)

Source: `internal/controller/virtualnetwork_controller.go`. For each enqueued vnet, `Reconcile`:

1. Records duration and outcome for the metrics (`defer observeReconcile`).
2. Fetches the vnet. If it is gone or being deleted, `deleteMembershipPolicies` (with no keep-set) removes every policy carrying its `kube-vnet.system/network` label, in every namespace.
3. Snapshots the prior `Ready`/`Degraded` status so transitions can emit Events.
4. Validates the name (DNS-1123 label; defense in depth behind the CRD's CEL rule, [ADR 0017](../adr/0017-name-validation-via-cel-and-runtime-check.md)). On failure: `Ready=False`/`Degraded=True`, reason `InvalidName`.
5. Checks the home namespace via `NSFilter.IsManaged`; system vnets are exempt. If unmanaged: `HomeNamespaceExcluded`, and leftover policies are cleaned up.
6. **Discovers members** (`discoverMembers`) over all pods:
   - An advisory scan of `kube-vnet/net.*` join labels records `InvalidJoiner`s (`UnknownDirection`, `NamespaceExcluded`) for the `Degraded` condition, for pods in namespaces the vnet admits only. The bare form counts only in the vnet's home namespace (from anywhere for `cluster`), so a bad `kube-vnet/net.namespace` label degrades only the pod's own `namespace` vnet. It never decides membership.
   - Membership comes only from the stamped `kube-vnet.system/net.*` label. Pods without the `resolved-generation` annotation are skipped (fail-closed during the stamping window), and a stamp is trusted only if the pod's namespace is still managed and still permitted by `allowedNamespaces`.
7. Generates the desired policies (`Generate`).
8. Applies each with `applyPolicyAndDetectRestore`: an uncached `Get` detects a missing policy (→ `PolicyRestored` Event on the vnet and the policy, if the stored `status.generatedPolicies` listed it), then server-side apply with field owner `kube-vnet` and `ForceOwnership`, skipped if the live policy already matches. A failed apply doesn't stop the loop: it counts `kube_vnet_apply_errors_total{kind="membership_policy"}`, emits an `ApplyFailed` Event on the desired policy (in the member namespace), and the remaining namespaces are still applied. One summary `ApplyFailed` goes on the vnet.
9. Deletes stale policies (`deleteMembershipPolicies` with the desired set as keep-set): this vnet's policies no longer in the desired set. This runs even if an apply failed, but spares the namespaces where one did.
10. Sets `Degraded` (`InvalidJoiners` or `NoIssues`) and `Ready` (`ApplyFailed`, `NoMembers` or `PoliciesGenerated`), writes status only if it changed, and emits transition Events.
11. Updates `kube_vnet_members_total`. Returns the joined apply errors if any failed (retry with backoff), otherwise `RequeueAfter: 10m` as a safety-net resync.

---

## Reconciler ownership and watches

Each reconciler watches everything it reads ([ADR 0044](../adr/0044-trigger-sets-must-cover-read-sets.md)); the exact trigger sets are in [code-structure § reconciler boundaries](code-structure.md#reconciler-boundaries).

The split is deliberate. The `VirtualNetworkReconciler` is purely about membership; the `NamespaceReconciler` is purely about the baseline and never inspects vnets. Per-namespace variation comes from which vnets the *pods* belong to, not from per-namespace baseline config. See [ADR 0023](../adr/0023-decoupled-disabled-and-ingress-isolation.md), [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md), [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md).

The `VirtualNetworkReconciler`'s pod watch uses `handler.Funcs` rather than a map function because removals matter: when a pod loses a label, only the *old* object says which vnet it left, so the update handler enqueues the union of vnets referenced by old and new ([ADR 0013](../adr/0013-pod-watch-with-handler-funcs-for-removals.md)). Its predicate passes only changes to `kube-vnet/net.*` or `kube-vnet.system/net.*` labels or the `resolved-generation` annotation.

The informer resync period is controller-runtime's default (10 hours); the 10-minute `RequeueAfter` above is what keeps vnets fresh.

---

## Server-side apply with field manager

Every operator-managed object is written with `client.Apply`, `client.FieldOwner("kube-vnet")` and `client.ForceOwnership`:

- Drift correction is automatic: a hand-added allow rule on an operator policy is removed on the next apply.
- User-managed `NetworkPolicy` objects are separate objects and unaffected; NetworkPolicies are ORed, so user policies compose additively.
- Create-or-update is one call, with no optimistic-concurrency loop.
- `NetworkPolicy` writes go through `applyPolicy` (`sweep.go`), which first reads the live policy and skips the apply when spec, owner references and labels already match and every desired annotation is present. A steady-state reconcile writes nothing; an edited policy no longer matches and is re-applied.

Details: [ADR 0009](../adr/0009-server-side-apply-with-field-manager.md).

---

## Cross-namespace cleanup via the network label

Kubernetes has no cross-namespace owner references. Every membership policy carries `kube-vnet.system/network=<homeNS>.<vnet>`, and `deleteMembershipPolicies` deletes by that label cluster-wide. The home-namespace policy additionally has an owner reference to the vnet, so garbage collection covers it too. See [ADR 0010](../adr/0010-cross-namespace-cleanup-via-network-label.md).

---

## Drift correction

Each family is restored by the reconciler that owns it, triggered by a watch on its own policies:

| Policy | Watch mapping | Restored by | Event |
|---|---|---|---|
| Membership (`kube-vnet.mem.*`) | `kube-vnet.system/network` label → owning vnet | `VirtualNetworkReconciler` | `PolicyRestored` if it had been deleted |
| Baseline (`kube-vnet.base`) | `role=baseline` → its namespace | `NamespaceReconciler` | none |
| Auto-allow (`kube-vnet.ext.*`) | owner reference → Service, or namespace for `ext.host` | the owning auto-allow reconciler | none |
| System vnets | `kube-vnet.system/managed-by` → namespace | `SystemVnetReconciler` | none |

The window between deletion and restore is usually sub-second to a few seconds; during it, traffic the policy would have denied is allowed. Hard isolation against namespace owners with NetworkPolicy-delete RBAC requires `AdminNetworkPolicy` ([ADR 0019](../adr/0019-baseline-durability.md)).

---

## Metrics collector

`MetricsCollector` lists `VirtualNetwork`s and operator-managed `NetworkPolicy`s every 30 seconds and sets `kube_vnet_networks_total` and `kube_vnet_managed_policies_total`. These are cluster-wide properties, so they are kept off the per-vnet reconcile path. The other four are updated by the `VirtualNetworkReconciler`, except `kube_vnet_apply_errors_total{kind="baseline"}`, which the `NamespaceReconciler` increments. Full list: [`metrics-and-events.md`](../reference/metrics-and-events.md).

---

## Where to read more

- Accepted decisions: [`adr/`](../adr/README.md).
- File-by-file source map and data flow: [`code-structure.md`](code-structure.md).
- Historical long-form rationale: [`design.md`](design.md).
- Labels, annotations, metrics, events, condition reasons: [`reference/`](../reference/).
