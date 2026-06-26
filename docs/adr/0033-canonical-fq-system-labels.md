# 0033 — Canonical fully-qualified system labels; per-binding policies removed

> **Note (ADR 0039 amendment, 2026-06-26)**: the policy name shape `kube-vnet.<homeNS>.<vnet>-<8hex>` referenced throughout this ADR is now `kube-vnet.mem.<homeNS>.<vnet>-<8hex>`. The cluster singleton bare-name treatment is preserved inside the new `mem.` prefix: `kube-vnet.cluster-<hash>` → `kube-vnet.mem.cluster-<hash>`. The label keys themselves (`kube-vnet.system/net.*`) are unchanged. See [ADR 0039](0039-uniform-kind-prefixed-policy-naming.md).

Status: Accepted

Date: 2026-05-06

Refines: [ADR 0011](0011-policy-naming-and-truncation.md), [ADR 0022](0022-long-form-join-label-in-home-namespace.md), [ADR 0026](0026-virtualnetworkbinding-crd.md). The truncate-and-hash logic from 0011 survives unchanged; what changes is the *name shape* fed into it.

## Context

The operator output before this ADR carried a bare/prefixed split that mirrored the pod-input ergonomic from [ADR 0022](0022-long-form-join-label-in-home-namespace.md). A pod in vnet `payments`'s home namespace `platform` could express membership as either `kube-vnet/net.payments=both` (bare) or `kube-vnet/net.platform.payments=both` (prefixed). The operator stamped whichever form the pod used, and the policy generator emitted up to two membership policies per `(vnet, namespace)` — one selecting on the bare system label, one on the prefixed.

That split exists *only* because the pod-input form has two shapes. The output didn't have to mirror the input. Two policies for one vnet in one namespace exists for no semantic reason — they're redundant in coverage.

A second leftover from before the resolution layer: bindings produce dedicated `kube-vnet.<homeNS>.<vnet>.b.<binding>-<8hex>` policies. Under [ADR 0030](0030-unified-vnet-membership-with-resolution.md)'s resolution model, `bindingRules` already stamps `kube-vnet.system/net.<vnet>` on selected pods. The regular per-vnet membership policy's selector matches those pods. The dedicated per-binding policy duplicates coverage.

A third issue surfaces when looking at this: `Degraded.reason=ConflictingDirections` (from `internal/controller/virtualnetwork_controller.go:436-450`) fires only when a pod has both bare and prefixed labels for the same vnet with different directions. Under canonicalization, both inputs map to the same VnetKey at stamp time, and the resolver's intersection (per ADR 0031) handles disagreements without a degraded condition. The reason is obsolete in its current form, but a generalized resolution-conflict surface (binding-vs-label, binding-vs-binding) is still valuable.

The intended outcome: every pod-tier system label and every membership-policy name is uniformly `<homeNS>.<vnet>` (FQ). User input ergonomics survive (ADR 0022's bare-label-in-home-NS still works). Baseline-spec keys stay bare for the chart-values shorthand and the `--elide-baseline-for` flag — the operator translates bare to FQ at render time. Per-binding policies are deleted. `ConflictingDirections` becomes `ResolutionConflict`, sourced from the resolver's existing `Conflicts` slice.

## Decision

### One canonical key per pod-tier membership

`canonicalVnetKey(ref, podNS) → VnetKey` returns the FQ form for every membership, including system vnets:

- User vnet: `<ref.Namespace>.<ref.Name>`
- System vnet `namespace`: `<podNS>.namespace` (the per-NS namespace vnet lives in the pod's NS)
- System vnet `cluster`: `cluster` (bare — see Amendment below)

`podLabelRules` normalizes both bare and prefixed pod-input labels through this helper. `bindingRules` and the baseline-rule paths also use it (already did, for user vnets). The pod-stamped `kube-vnet.system/net.<key>` is therefore always FQ.

### One policy per (vnet, namespace)

`PolicyName(vnet, homeNS) → kube-vnet.<homeNS>.<vnet>-<8hex>` is the single naming function. The `KeyForm` enum, `PolicyNameFor`, `JoinLabelKeyByForm`, `SystemLabelKeyByForm`, and the `LabelBinding`/`kube-vnet/binding=<binding>` label are all deleted. The membership policy's selector matches the canonical FQ system label.

System-vnet policies go through the same path: cluster vnet's `homeNS` is the operator's release NS, per-NS namespace vnets each have `homeNS = thisNS`. No special-casing.

### Baseline elide-list translation

`DesiredBaseline` in `baseline.go` translates `--elide-baseline-for` entries from bare to FQ at render time. Per-NS baselines render their own per-NS keys for `namespace`. Bare entries in the chart's `clusterBaseline.memberships` map shorthand work the same way: the chart helper template translates them at render time, or the resolution controller does it on read.

### `ResolutionConflict` replaces `ConflictingDirections`

`ReasonConflictingDirections` deleted. `ReasonResolutionConflict` added. Sourced from `ResolutionResult.Conflicts` (already populated by the resolver per ADR 0030 / ADR 0031). Any non-empty conflicts list naming this vnet produces a `Degraded=true reason=ResolutionConflict` condition with a message identifying the conflicting sources and the intersected effective direction. Per-pod `kube-vnet.system/conflict.<vnet>` annotations and the `kube_vnet_resolution_conflicts_total` metric continue as the granular surfaces.

### Hard cleanup guarantees

Two reconcilers gain explicit cleanup tail-steps. No orphan window, ever.

- `VirtualNetworkReconciler`: every successful reconcile lists policies labeled `kube-vnet.system/network=<homeNS>.<vnet>` cluster-wide, computes the desired set (one per `(vnet, member-NS)`), and deletes the difference. Catches the bare-form-policy and per-binding-policy migration cleanup deterministically.
- `SystemVnetReconciler`: gains `delete` verb. On a Namespace event for a now-disabled namespace, lists `VirtualNetwork`s in that NS labeled `kube-vnet.system/managed-by=kube-vnet` and deletes them. The reserved-name VAP guarantees only the operator could have created such a vnet, so deletion is safe.

The `NamespaceReconciler` (baseline lifecycle) is already symmetric (creates baseline on managed transition, no-ops on disabled). No change needed.

### Amendment: cluster singleton exception

The cluster system vnet is THE cluster-wide singleton — there's only ever one, anchored on the operator's release namespace by definition. The reserved-name VAP forbids user-authored vnets named `cluster` in any namespace, so the suffix `cluster` is unambiguous on its own. The `<operatorNS>.` prefix carries no information.

For cluster, the canonicalization rule **inverts**: any input form (bare `cluster` or prefixed `<anything>.cluster`) collapses to bare `cluster`. Specifically:

- Pod-stamped system label: `kube-vnet.system/net.cluster=<dir>` (not `kube-vnet.system/net.<operatorNS>.cluster`).
- Membership policy name: `kube-vnet.cluster-<8hex>` (not `kube-vnet.<operatorNS>.cluster-<8hex>`).
- Baseline elide-list translation: `cluster` → `kube-vnet.system/net.cluster` (the `CanonicalSuffix` rule does the collapse automatically; `DesiredBaseline` is unchanged).

Cluster is the only vnet where the suffix is *removed* rather than *added* — every other vnet stamps FQ for disambiguation; cluster has nothing to disambiguate. The `kube-vnet.system/network=<operatorNS>.cluster` cleanup label is intentionally retained on policies (it's a routing/identity tag, not a selector key — operators querying by `<operatorNS>.cluster` keep working).

Migration: existing FQ-stamped pods get re-stamped to bare on next resolution; the cleanup tail-step removes the old `kube-vnet.<operatorNS>.cluster-<hash>` policies and emits the new `kube-vnet.cluster-<hash>`-named ones.

## Consequences

- One membership policy per `(vnet, namespace)` instead of up to two-or-more. Cleaner `kubectl get networkpolicy` output, simpler mental model.
- Per-binding policies gone. Bindings still drive memberships — they stamp the system label on selected pods like everything else does. Binding-targeted pods are covered by the regular per-vnet membership policy.
- `kube-vnet/binding=<binding>` policy label gone (no per-binding policies to label).
- `ConflictingDirections` Degraded reason gone; `ResolutionConflict` takes its place for cross-source conflicts that intersection resolves but humans should still see.
- Existing pods with old bare-form `kube-vnet.system/net.<vnet>` labels: re-stamped with FQ on first resolution; old labels stripped via `applyResolution`'s desired-set diff. No special migration code.
- Existing membership policies named `kube-vnet.<vnet>-<8hex>` (bare) or `kube-vnet.<homeNS>.<vnet>.b.<binding>-<8hex>` (per-binding): deleted by the new cleanup tail-step on first reconcile after upgrade.
- Existing per-NS `namespace` system vnets in (now-) disabled namespaces: deleted by the SystemVnetReconciler change on next reconcile.
- ADR 0022's bare-label-in-home-NS input ergonomic survives unchanged — both `kube-vnet/net.foo` and `kube-vnet/net.<homeNS>.foo` continue to be accepted on pod templates. Helm-templated workloads using the same single label key across namespaces still work.

## Alternatives considered

- **Keep bare-only stamping; drop prefixed input.** Rejected: breaks ADR 0022's templating ergonomic. Helm-chart pod templates that use a single `kube-vnet/net.foo` label across namespaces would no longer work in foreign namespaces.
- **Stamp both bare and prefixed on every pod.** Rejected: doubles the label noise, and the policy generator still has to pick one to selector-match. Doesn't simplify the mental model, just shifts the duplication.
- **Keep per-binding policies as a debugging aid.** Rejected: `VirtualNetworkBinding.status.AttachedPods` already enumerates which pods the binding selects. The per-binding policy adds nothing observability-wise that the binding's status doesn't already have.
- **Drop `ConflictingDirections` without a replacement.** Rejected: binding-vs-label conflicts are a real signal users should see. The replacement is `ResolutionConflict`, sourced from data the resolver already produces.
- **CI lint to enforce the cleanup-on-every-reconcile pattern.** Worth doing eventually if managed-resource churn becomes routine; for now, the integration tests cover the specific stale-resource cases.

## Out of scope

- Renaming the `kube-vnet.system/net.` prefix itself. Only the suffix shape changes (always FQ); the prefix stays.
- `kube-vnet.system/resolved-generation` rename (separate prior thread; can fold into a follow-up).
- `--elide-baseline-for` syntax — unchanged. Bare entries (`cluster`, `namespace`) keep working; the operator translates to FQ.
- `AdminNetworkPolicy` integration. Not on the roadmap.
- Reworking `VirtualNetworkBinding.status` (`AttachedPods`, `Ready`). Bindings still produce status from their own reconciler; this ADR removes only the *separate policy* they used to emit.
