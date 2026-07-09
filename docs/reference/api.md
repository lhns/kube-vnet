# API reference: the four CRDs

Full field-level reference for all four kube-vnet CRDs, in this order:

1. [`VirtualNetwork`](#group--version--kind) — a named network
2. [`VirtualNetworkBinding`](#virtualnetworkbinding) — no-label pod attachment
3. [`VirtualNetworkBaseline`](#virtualnetworkbaseline) — namespace-tier defaults
4. [`ClusterVirtualNetworkBaseline`](#clustervirtualnetworkbaseline) — cluster-tier defaults

For the conceptual model see [`concepts.md`](../getting-started/concepts.md); this document is for look-up.

---

# VirtualNetwork

## Group / version / kind

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
```

- **API group**: `kube-vnet.lhns.de`
- **API version**: `v1alpha1`
- **Kind**: `VirtualNetwork`
- **Short names**: `vnet`, `vnets` (`kubectl get vnet -A`)
- **Scope**: Namespaced

---

## Resource shape

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: <string>             # required; DNS-1123 label (no dots)
  namespace: <string>        # required; the "home namespace"
spec:
  description: <string>      # optional; free text
  allowedNamespaces:         # optional; default: home namespace only
    all: <bool>              # optional
    names: [<string>, ...]   # optional
    selector:                # optional
      matchLabels: { ... }
      matchExpressions: [ ... ]
status:
  conditions: [ ... ]
  members: [ ... ]
  generatedPolicies: [ ... ]
  observedGeneration: <int>
```

---

## metadata

`metadata.name` must match the DNS-1123 label regex `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` — lowercase alphanumeric and hyphens, no dots, max 63 chars. Enforced at admission by an `x-kubernetes-validations` (CEL) rule on the CRD; defense-in-depth check at runtime in the reconciler. See [ADR 0017](../adr/0017-name-validation-via-cel-and-runtime-check.md).

`metadata.namespace` is the **home namespace**. The home namespace is always implicitly in `allowedNamespaces`; pods in the home namespace use the *bare* join label form.

---

## spec

### `spec.description` (optional)

Free-text human-readable description. The operator does not interpret it.

```yaml
spec:
  description: |
    Connects payments-related services within the platform namespace.
```

### `spec.allowedNamespaces` (optional)

Controls which namespaces' pods are *allowed to join* this network. **Does not grant blanket access** — pods in permitted namespaces still need to add the join label to become members.

If unset (the default), only the home namespace can join.

#### Sub-fields

```yaml
allowedNamespaces:
  all: <bool>
  names: [<string>, ...]
  selector: <metav1.LabelSelector>
```

| Field | Type | Default | Description |
|---|---|---|---|
| `all` | bool | false | When true, pods in any namespace may join. Wildcard form. When true, `names` and `selector` are ignored. |
| `names` | `[]string` | `[]` | Explicit list of namespace names allowed to join. Names matched exactly — no glob/regex. Use `selector` for groups. |
| `selector` | `metav1.LabelSelector` | nil | Standard Kubernetes label selector on the `Namespace` object. Pods in matching namespaces may join. |

The home namespace is always implicitly allowed. If multiple matchers are set they union — a namespace matches if any one of (`all`, `names`, `selector`) matches.

Examples:

```yaml
# Default: only the home namespace.
spec: {}

# Allow two specific foreign namespaces.
spec:
  allowedNamespaces:
    names: [webapp, monitoring]

# Allow any namespace labeled tier=prod.
spec:
  allowedNamespaces:
    selector:
      matchLabels:
        tier: prod

# Wildcard: any namespace.
spec:
  allowedNamespaces:
    all: true
```

See [ADR 0005](../adr/0005-namespaced-crd-with-allowed-namespaces.md) for the full rationale and the join-vs-blanket explanation.

---

## status

The operator writes status; users do not. Updated via the `/status` subresource.

### `status.conditions`

Standard Kubernetes condition pattern (`metav1.Condition`):

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: PoliciesGenerated
      message: "5 NetworkPolic(y|ies) across 2 namespace(s)"
      lastTransitionTime: "2026-04-30T17:24:13Z"
    - type: Degraded
      status: "False"
      reason: NoIssues
      message: ""
      lastTransitionTime: "2026-04-30T17:24:13Z"
```

Two condition types are maintained: `Ready` and `Degraded`.

### Ready condition

| Status | Reason | Message gist | When it fires |
|---|---|---|---|
| True | `NoMembers` | "no pods are joining this VirtualNetwork" | Reconcile succeeded; no pods carry the appropriate join label. |
| True | `PoliciesGenerated` | "<N> NetworkPolic(y|ies) across <M> namespace(s)" | Reconcile succeeded; at least one policy generated. |
| False | `InvalidName` | "name <name> is not a DNS-1123 label" | The name fails the runtime validation regex (the CRD's CEL rule should prevent this from being persisted; this is defense-in-depth). |
| False | `HomeNamespaceExcluded` | "home namespace <ns> is excluded by the operator" | The vnet's home namespace is in `--disabled-namespaces` or has `kube-vnet/disabled=true`. |
| False | `ApplyFailed` | apiserver error message | A `NetworkPolicy` apply call returned an error. |
| False | `NamespaceNotAllowed` | "..." | A vnet-level surface for the namespace-permission check; usually the per-pod `InvalidJoiners` reason on `Degraded` is what users see. |
| False | `NamespaceExcluded` | "..." | A vnet-level surface for namespace-exclusion; usually surfaces as `HomeNamespaceExcluded` when the home namespace itself is excluded. |

### Degraded condition

| Status | Reason | Message gist | When it fires |
|---|---|---|---|
| False | `NoIssues` | "" | Reconcile clean; no issues observed. |
| True | `InvalidJoiners` | "<N> invalid joiner(s): <ns/pod>, <ns/pod>, …" | Some pods carry the prefixed join label but their namespace is non-permitted (`NamespaceNotAllowed`) or excluded (`NamespaceExcluded`). The Degraded message names the offending pods. |
| True | `UnknownDirection` | "<N> pod(s) with unknown direction: <ns/pod>=<value>, …" | At least one pod's join-label value is not one of `both`/`ingress`/`egress`/`none` (the legacy `"true"`/`"false"`/empty aliases are rejected at admission on K8s >= 1.30 and excluded at reconcile time everywhere). The pod is excluded from membership. See [ADR 0021](../adr/0021-direction-modes-on-join-labels.md). |
| True | `ResolutionConflict` | "<N> pod(s) with cross-source resolution conflicts: <ns/pod>, …" | At least one pod has conflicting `Direction` values from two distinct resolution sources for this vnet (e.g. a binding says `both` while a pod label says `egress`, or two bindings disagree). The conflict is intersected fail-closed and the per-pod conflict annotation `kube-vnet.system/conflict.<homeNS>.<vnet>` is set. See [ADR 0033](../adr/0033-canonical-fq-system-labels.md). |
| True | `InvalidName` | as above | Mirrors the Ready / `InvalidName` case. |
| True | `HomeNamespaceExcluded` | as above | Mirrors the Ready / `HomeNamespaceExcluded` case. |
| True | `NameCollision` | (planned; tracked) | A user-managed `NetworkPolicy` with the same name kube-vnet wants to use exists and doesn't carry the `kube-vnet.system/managed-by` label. The operator refuses to overwrite. |

The full machine-readable reason constants live in `internal/controller/virtualnetwork_controller.go` (the `Reason*` block).

### `status.members`

Observed pod membership grouped by namespace. Updated on every successful reconcile. The shape is:

```yaml
status:
  members:
    - namespace: platform
      pods:
        - orders-7c5f4b-abc12
        - orders-7c5f4b-def34
    - namespace: webapp
      pods:
        - frontend-9d8e7-xyz98
```

Sorted by namespace name; pods within a namespace are sorted by name. Updates are not real-time; they happen on each reconcile after pod label changes propagate through the watch.

### `status.generatedPolicies`

References to the `NetworkPolicy` resources the operator has applied for this VirtualNetwork. Useful for debugging and as the source of truth for cleanup verification.

```yaml
status:
  generatedPolicies:
    - namespace: platform
      name: kube-vnet.mem.platform.payments-1a2b3c4d
    - namespace: webapp
      name: kube-vnet.mem.platform.payments-1a2b3c4d
```

This list does **not** include the `kube-vnet.base` baseline (see [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md)). The baseline is a namespace-level concern, not a per-vnet concern; it's tracked separately by labels (`kube-vnet.system/role=baseline`).

### `status.observedGeneration`

Standard Kubernetes pattern: `metadata.generation` last seen by the controller. Lets clients tell whether the status reflects the latest spec.

---

## Printer columns

`kubectl get vnet` shows:

| Column | Source |
|---|---|
| `NAME` | `metadata.name` |
| `READY` | `status.conditions[?(@.type=="Ready")].status` |
| `AGE` | `metadata.creationTimestamp` |

---

## Validation rules

- **Name**: DNS-1123 label (lowercase alphanumeric and hyphens; no dots; max 63 chars). Enforced via CRD-level `x-kubernetes-validations` CEL rule; runtime check in the reconciler as defense-in-depth.
- **`allowedNamespaces.names`**: must be valid namespace names (DNS-1123 label) — Kubernetes' standard validation. The operator does not re-validate.

There is currently no admission webhook. The CEL rule covers the only known invalid-name case. See [ADR 0017](../adr/0017-name-validation-via-cel-and-runtime-check.md).

---

## Lifecycle

| Event | What happens |
|---|---|
| Create | Reconciler enqueues; if no pods carry the join label, status becomes `Ready=True, NoMembers` and no policies are generated. |
| Pod added/labeled | Pod-watch fires; reconciler enqueues the relevant vnet(s); membership updated; policy created/updated; baseline installed in the namespace if it wasn't already. |
| Pod removed/un-labeled | Same as above; membership updated; policy may shrink (peer rules) or be deleted (if the namespace empties); baseline GC'd if the namespace has no managed members. |
| Spec edit | Reconciler enqueues; new desired state computed; SSA reconciles; stale policies (e.g. for namespaces no longer in `allowedNamespaces`) deleted. |
| Delete | `cleanupForDeleted` lists policies cluster-wide by `kube-vnet.system/network=<homeNS>.<name>` and deletes them all (including in foreign namespaces). Baseline GC'd in each touched namespace. |

For the full reconciliation algorithm see [`../architecture.md`](../internals/architecture.md).

---

## Compatibility

- **CRD apiVersion**: `kube-vnet.lhns.de/v1alpha1`. Breaking changes are allowed across alpha versions (no `Conversion` webhook is provided). Pin to a specific operator version in production.
- **Minimum Kubernetes version**: 1.25 (CEL validation rules are GA in 1.25).
- **Field deprecations / removals**: announced in `CHANGELOG.md` and a corresponding ADR.

---

# VirtualNetworkBinding

A namespaced CRD that selects pods *in its own namespace* and attaches them to a target `VirtualNetwork`. Used when you can't add a join label to the pod template (third-party Helm charts, pods owned by another operator). See [ADR 0026](../adr/0026-virtualnetworkbinding-crd.md).

## Group / version / kind

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBinding
```

- **API group**: `kube-vnet.lhns.de`
- **API version**: `v1alpha1`
- **Kind**: `VirtualNetworkBinding`
- **Short names**: `vnb`, `vnbs` (`kubectl get vnb -A`)
- **Scope**: Namespaced

## Resource shape

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBinding
metadata:
  name: <string>
  namespace: <string>            # the binding's own namespace; selector is scoped here
spec:
  virtualNetworkRef:
    name: <string>               # required
    namespace: <string>          # required (target vnet's home namespace)
  direction: both                # both | ingress | egress | none; defaults to both
  podSelector:                   # required; standard metav1.LabelSelector
    matchLabels: { ... }
    matchExpressions: [ ... ]
status:
  conditions: [ ... ]            # Ready
  attachedPods: [<string>, ...]  # pod names selected (in the binding's namespace)
  observedGeneration: <int>
```

## spec

| Field | Type | Description |
|---|---|---|
| `virtualNetworkRef.name` | string | Name of the target `VirtualNetwork`. Required. |
| `virtualNetworkRef.namespace` | string | Home namespace of the target `VirtualNetwork`. Required. |
| `direction` | string enum | `both` (default) \| `ingress` \| `egress` \| `none`. Same enum as the join label value. |
| `podSelector` | `metav1.LabelSelector` | Required. **Scoped to the binding's own namespace** — there are no cross-namespace bindings. |

The target vnet's `spec.allowedNamespaces` is enforced. A binding in a non-permitted namespace surfaces `Ready=False, Reason=NamespaceNotAllowed`. A binding in a `kube-vnet/disabled` (or operator-excluded) namespace is inert (`Ready=False, Reason=NamespaceExcluded`).

## status

### `Ready` condition

| Status | Reason | Meaning |
|---|---|---|
| True | `PodsAttached` | The selector matched at least one pod and the binding is producing the corresponding membership policy. `attachedPods` lists the pod names. |
| False | `NoPodsMatch` | The selector is valid but matched zero pods in the binding's namespace. |
| False | `VirtualNetworkNotFound` | `spec.virtualNetworkRef` does not resolve. |
| False | `NamespaceNotAllowed` | The target vnet's `spec.allowedNamespaces` does not permit the binding's namespace. |
| False | `NamespaceExcluded` | The binding's namespace has `kube-vnet/disabled=true` or is in `--disabled-namespaces`. |
| False | `UnknownDirection` | `spec.direction` is not one of the recognized values. |
| False | `InvalidSelector` | `spec.podSelector` is not a parseable label selector. |

The Go-level reason constants live in `internal/controller/virtualnetworkbinding_controller.go` (the `ReasonBinding*` block).

### `attachedPods`

A sorted list of pod names (in the binding's namespace) selected by `spec.podSelector`. Refreshed on every successful reconcile. Useful for catching too-broad selectors.

### `observedGeneration`

Standard Kubernetes pattern: `metadata.generation` last seen by the controller.

## Generated NetworkPolicy

No per-binding policy is emitted (per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)). The resolution controller stamps the canonical FQ system label `kube-vnet.system/net.<homeNS>.<vnet>=<direction>` on every pod selected by the binding, and the regular per-`(vnet, namespace)` membership policy `kube-vnet.<homeNS>.<vnet>-<8hex>` covers them via the standard selector.

The binding controller writes only the binding's status. The desired-state computation in `VirtualNetworkReconciler` watches bindings and folds them into the regular membership policy.

## Lifecycle

| Event | What happens |
|---|---|
| Create | Binding controller validates spec, computes the matching pod set, sets `Ready` accordingly, writes status. The owning vnet is enqueued so its policy set picks up the binding. |
| Pod added/removed in binding's namespace | Binding's controller refreshes `attachedPods`. The resolution controller adjusts each pod's stamped system labels; the regular membership policy reflects the new peer set on next vnet reconcile. |
| Spec edit | Same as create. |
| Delete | The resolution controller un-stamps the binding's contribution from each affected pod. The vnet's `deleteStale` step removes any policy no longer in the desired set. |

## Compatibility

- Same `kube-vnet.lhns.de/v1alpha1` versioning as `VirtualNetwork`.
- Bindings are an additive feature; existing label-driven membership continues to work unchanged.

---

# VirtualNetworkBaseline

The namespace-wide tier default: memberships that every pod in the namespace inherits, unless a lower tier (binding or pod label) overrides them. Singleton per namespace. See [ADR 0031](../adr/0031-baseline-tier-resolution.md) for the tier model.

## Group / version / kind

| Field | Value |
|---|---|
| Group | `kube-vnet.lhns.de` |
| Version | `v1alpha1` |
| Kind | `VirtualNetworkBaseline` |
| Scope | Namespaced |
| Short names | `vnbl`, `vnbls` |
| Singleton | must be named `default` (CEL-enforced at admission) |

## Resource shape

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBaseline
metadata:
  name: default            # only accepted name
  namespace: webapp
spec:
  memberships:
    - virtualNetworkRef:
        name: payments
        namespace: platform
      direction: default-ingress
```

## spec

| Field | Type | Required | Meaning |
|---|---|---|---|
| `memberships` | `[]BaselineMembership` | no (empty = no NS-tier defaults) | Memberships every pod in this namespace inherits. Atomic list; order not significant. |
| `memberships[].virtualNetworkRef.name` | string | yes | Target vnet's name. |
| `memberships[].virtualNetworkRef.namespace` | string | yes | Target vnet's home namespace. |
| `memberships[].direction` | string | yes | One of the eight direction values: `both`, `ingress`, `egress`, `none`, `default-both`, `default-ingress`, `default-egress`, `default-none` (enum-enforced). Bare values are **enforced** — bindings and pod labels in this namespace cannot override them. `default-*` values are **advisory** — lower tiers may override per vnet. |

This baseline itself inherits from the [`ClusterVirtualNetworkBaseline`](#clustervirtualnetworkbaseline): it may override only entries the cluster baseline marked `default-*`. An attempt to override a bare cluster-pinned value is rejected (see `OverrideRejected` below); the cluster value stays in effect.

## status

| Condition type | Meaning |
|---|---|
| `Ready` | Baseline validated and folded into resolution. |
| `Conflicts` | Two entries reference the same vnet with disagreeing directions. Resolution still proceeds fail-closed (directions intersect); the condition surfaces the disagreement. |
| `OverrideRejected` | This baseline tried to override a vnet the cluster baseline pinned with a **bare** direction. The entry is ignored, the cluster value applies, and the condition message names the vnet. |

`status.observedGeneration` mirrors the reconciled `metadata.generation`.

## Printer columns

`kubectl get vnbl` shows `Ready` and `Age`.

## Lifecycle

| Event | What happens |
|---|---|
| Create / spec edit | Every pod in the namespace re-resolves; stamped `kube-vnet.system/net.*` labels and membership policies update. |
| Delete | The namespace falls back to the cluster baseline's defaults; pods re-resolve. |

---

# ClusterVirtualNetworkBaseline

The cluster-wide tier default — the root of the inheritance chain. Singleton per cluster. The Helm chart seeds it from `operator.clusterBaseline.ingressIsolationLevel` (or an explicit `memberships` map); it is annotated `helm.sh/resource-policy: keep` so `helm uninstall` leaves it in place.

## Group / version / kind

| Field | Value |
|---|---|
| Group | `kube-vnet.lhns.de` |
| Version | `v1alpha1` |
| Kind | `ClusterVirtualNetworkBaseline` |
| Scope | Cluster |
| Short names | `cvnbl`, `cvnbls` |
| Singleton | must be named `default` (CEL-enforced at admission) |

## Resource shape

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: ClusterVirtualNetworkBaseline
metadata:
  name: default             # only accepted name
spec:
  memberships:
    - virtualNetworkRef:
        name: namespace      # per-NS system vnet
        namespace: kube-vnet-system
      direction: default-both
    - virtualNetworkRef:
        name: cluster        # cluster-wide system vnet
        namespace: kube-vnet-system
      direction: default-egress
```

(The example above is exactly what `ingressIsolationLevel: namespace` seeds; see [the configuration reference](configuration.md) for all three presets.)

## spec

Identical shape to `VirtualNetworkBaseline.spec` — `memberships[]` with `virtualNetworkRef` + the eight-value `direction` enum. Semantics of bare vs `default-*` are the same, applied one tier higher: a **bare** direction here binds the entire cluster (namespace baselines, bindings, and pod labels cannot override it); `default-*` lets namespaces and pods adjust.

## status

| Condition type | Meaning |
|---|---|
| `Ready` | Baseline validated and folded into resolution. |
| `Conflicts` | Duplicate vnet refs with disagreeing directions; resolution intersects fail-closed. |

(No `OverrideRejected` — there is no tier above this one.)

## Printer columns

`kubectl get cvnbl` shows `Ready` and `Age`.

## Lifecycle

| Event | What happens |
|---|---|
| Create / spec edit | Every pod in every managed namespace re-resolves. This is the cluster-wide isolation-posture knob — changes here are cluster-wide events. |
| Delete | All cluster-tier defaults disappear; pods keep only namespace-baseline / binding / label memberships. Under the strict interpretation this means "everything not explicitly joined becomes isolated" — treat deletion as a deliberate posture change. |

## Compatibility

Both baseline CRDs ship in the same chart and version as `VirtualNetwork`. They replaced the removed `ClusterVirtualNetworkBinding` CRD and `--default-memberships` flag ([ADR 0031](../adr/0031-baseline-tier-resolution.md)).

---

# Validating manifests with kubeconform

kube-vnet publishes JSON Schemas for its four CRDs so you can validate kube-vnet custom resources with [kubeconform](https://github.com/yannh/kubeconform) in your own repos and CI — the same way you validate core Kubernetes objects. Without them, kubeconform has no schema for `kube-vnet.lhns.de` kinds and can only `-skip` them (no validation).

The schemas live in this repo under `schemas/<group>/<kind>_<version>.json` (the [datreeio/CRDs-catalog](https://github.com/datreeio/CRDs-catalog) layout) and are served directly over `raw.githubusercontent.com`. Point kubeconform at them with a templated `-schema-location`:

```bash
kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/lhns/kube-vnet/v0.5.1/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  my-virtualnetwork.yaml
```

- The first `-schema-location default` resolves core kinds (Deployment, Namespace, …); the second resolves the kube-vnet kinds. Keep both.
- The example pins the ref to a release tag (`v0.5.1`, the first release to ship `schemas/`) so validation is reproducible and matches the CRD version your cluster runs — bump it when you upgrade kube-vnet. Use `main` instead to always track the latest CRDs.
- The schemas are faithful to the CRDs: they enforce field types, `enum`s (e.g. `direction: both|ingress|egress|none`), `required` fields, and — because kube-vnet's CRDs are closed structural schemas — they reject **misspelled/unknown fields** (`additionalProperties: false`), so a typo like `podSelctor` fails validation. (The CEL-based cross-field rules such as "`podSelector` must be non-empty" are enforced by the apiserver, not expressible in JSON Schema, so kubeconform won't catch those.)

Maintainers: the schemas are generated from `config/crd/bases/*.yaml` by `scripts/gen-schemas.sh` and kept in sync by a CI drift-gate. `make manifests` regenerates them alongside the CRDs (or run `make schemas` on its own), so any API change picks them up automatically.
