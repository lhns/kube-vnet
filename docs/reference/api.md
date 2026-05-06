# API reference: VirtualNetwork and VirtualNetworkBinding

Full reference for the `VirtualNetwork` and `VirtualNetworkBinding` CRDs. For the conceptual model see [`../concepts.md`](../concepts.md); this document is for look-up.

---

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
| True | `UnknownDirection` | "<N> pod(s) with unknown direction: <ns/pod>=<value>, …" | At least one pod's join-label value is not one of `both`/`ingress`/`egress`/`none` (or the legacy `"true"`/`"false"`). The pod is excluded from membership. See [ADR 0021](../adr/0021-direction-modes-on-join-labels.md). |
| True | `ResolutionConflict` | "<N> pod(s) with cross-source resolution conflicts: <ns/pod>, …" | At least one pod has conflicting `Direction` values from two distinct resolution sources for this vnet (e.g. a binding says `both` while a pod label says `egress`, or two bindings disagree). The conflict is intersected fail-closed and the per-pod conflict annotation `kube-vnet.system/conflict.<homeNS>.<vnet>` is set. See [ADR 0033](../adr/0033-canonical-fq-system-labels.md). |
| True | `InvalidName` | as above | Mirrors the Ready / `InvalidName` case. |
| True | `HomeNamespaceExcluded` | as above | Mirrors the Ready / `HomeNamespaceExcluded` case. |
| True | `NameCollision` | (planned; tracked) | A user-managed `NetworkPolicy` with the same name kube-vnet wants to use exists and doesn't carry the `kube-vnet/managed-by` label. The operator refuses to overwrite. |

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
      name: kube-vnet-payments-platform
    - namespace: webapp
      name: kube-vnet-payments-webapp
```

This list does **not** include the `kube-vnet` baseline (see [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md)). The baseline is a namespace-level concern, not a per-vnet concern; it's tracked separately by labels (`kube-vnet/role=baseline`).

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
| Delete | `cleanupForDeleted` lists policies cluster-wide by `kube-vnet/network=<homeNS>.<name>` and deletes them all (including in foreign namespaces). Baseline GC'd in each touched namespace. |

For the full reconciliation algorithm see [`../architecture.md`](../architecture.md).

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
