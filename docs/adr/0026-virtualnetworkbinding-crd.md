# 0026 — `VirtualNetworkBinding` CRD as the no-label alternative

Status: Accepted

## Context

The primary mechanism for joining a `VirtualNetwork` is the join label on the pod (or, more usefully, on the pod template of a workload controller). That works when you own the manifests. It doesn't work when:

- The pod template comes from an upstream Helm chart you don't want to fork.
- The pods are owned by another operator (cert-manager, an Argo controller, a CNPG cluster, etc.) that re-templates them on every reconcile and would clobber added labels.
- A cluster admin needs to attach pods to a vnet without changing the team-owned manifests.

[ADR 0022](0022-long-form-join-label-in-home-namespace.md) explicitly rules out operator mutation of user pod labels. So the gap remains: there has to be *some* way to enroll pods without touching them.

## Decision

A new namespaced CRD, `VirtualNetworkBinding` (short names `vnb;vnbs`):

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBinding
metadata:
  name: payments-thirdparty
  namespace: webapp
spec:
  virtualNetworkRef:
    name: payments
    namespace: platform
  direction: both                # both|ingress|egress|none, defaults to both
  podSelector:                   # required, scoped to metadata.namespace
    matchLabels:
      app: thirdparty-billing-agent
status:
  conditions:
    - type: Ready
      status: "True"
      reason: PodsAttached
      message: "3 pod(s) attached to platform/payments"
  attachedPods: [app-7c5..., app-7c5..., app-7c5...]
  observedGeneration: 1
```

### Constraints

- **Same-namespace selector.** The binding selects pods *in its own namespace* — the `podSelector` is implicitly scoped to `metadata.namespace`. Cross-namespace bindings would defeat the point of `spec.allowedNamespaces` on the target vnet.
- **`allowedNamespaces` is enforced.** A binding in a namespace not permitted by the target vnet's `spec.allowedNamespaces` surfaces `Ready=False, Reason=NamespaceNotAllowed`. The vnet does not honor it.
- **Disabled namespaces are inert.** A binding in a namespace with `kube-vnet/disabled=true` (or in the operator's exclusion list) is ignored. The binding's own status reflects this with `Ready=False, Reason=NamespaceExcluded`.
- **Direction conflicts surface.** A binding with an unknown direction value reports `Ready=False, Reason=UnknownDirection` and contributes nothing.

### How it lands in the generator

The generator emits **one extra membership policy per binding**, in the binding's own namespace, whose `podSelector` is the binding's verbatim `spec.podSelector`. The policy's ingress/egress shape follows the binding's direction. Peer rules in this binding-driven policy include the standard label-driven peer entries plus an entry per other binding (selector + namespace).

In parallel, every label-driven membership policy gets one *additional* `peer` entry per binding (so label-driven members can reach binding-driven members and vice versa). NetworkPolicy doesn't support OR-ing selectors within a single peer entry, so additional entries are the only way.

Policies are named `kube-vnet-<vnet>-b-<binding>` and labeled `kube-vnet/binding=<binding>` for traceability.

### Why a separate CRD rather than another label form

A second label form (e.g. `kube-vnet/net-binding.<vnet>=true`) would have the same not-able-to-modify-the-pod-template problem the binding is meant to solve. The whole point is to enroll pods *without* writing to them. A CRD is the natural fit: a separate object the operator owns, in the binding's namespace, observed and acted on by the operator.

### Why "VirtualNetworkBinding" over `Member` / `Attachment` / `Membership`

Follows the `RoleBinding` pattern: a binding ties some subjects (selected pods) to a named abstraction (the target vnet). `Attachment` echoes Multus' `NetworkAttachmentDefinition` but that resource is about CNI plugin selection — wrong shape. `Member`/`Membership` reads as a stamp on the pod, which would be misleading: bindings select pods, they don't mark them.

## Consequences

- **Pro**: Closes the can't-modify-the-pod-template gap without operator-driven label mutation.
- **Pro**: `RoleBinding`-shaped — familiar pattern; printer columns (`VNet`, `VNet-NS`, `Direction`, `Ready`) make `kubectl get vnb -A` immediately useful.
- **Pro**: Per-binding policy traceability (`kube-vnet/binding=<name>` label) makes troubleshooting straightforward.
- **Con**: Policy count grows linearly with the number of bindings (one extra membership policy per binding) and peer-rule count grows by one entry per binding in every other policy. At very high binding counts (many hundreds across one vnet) the resulting NetworkPolicy objects can get large. Mitigation: the standard label form is still the recommended primary mechanism; bindings are an escape hatch, not the everyday path.
- **Con**: A binding's selector is opaque to the operator at write time — if the user's selector is too broad, more pods get attached than expected. `status.attachedPods` makes this visible; reviewing it after creating a binding catches the mistake.
- **Con**: Two ways to express membership (label vs binding) means two surfaces to document. Worth it for the use case.

## Cross-references

- ADR 0021 — Direction modes on join labels. The `direction` field on the binding uses the same enum.
- ADR 0022 — Long-form join label in home namespace; explicit no-mutation stance that motivates this ADR.
- ADR 0005 — Namespaced CRD with `allowedNamespaces`. Bindings are subject to the same allow rule.

## Addendum 2026-05-05 — `ClusterVirtualNetworkBinding` and resolution-driven semantics

[ADR 0030](0030-unified-vnet-membership-with-resolution.md) extends bindings in two ways:

1. **New cluster-scoped sibling: `ClusterVirtualNetworkBinding`.** Same shape as `VirtualNetworkBinding` but cluster-scoped, with a `namespaceSelector` field added so a single cluster-wide binding can target pods across all (or a subset of) managed namespaces. Naming follows K8s convention (`ClusterRole` vs `Role`).

2. **Bindings stop being a parallel mechanism.** Previously the policy generator handled bindings specially, generating per-binding NetworkPolicies that lived alongside label-driven membership policies. Under ADR 0030, both `VirtualNetworkBinding` and `ClusterVirtualNetworkBinding` are inputs to the resolution controller, which translates them into `kube-vnet.system/net.<vnet>=<direction>` labels stamped on the matched pods. The generator only ever sees labels.

Empty `podSelector: {}` in a namespaced `VirtualNetworkBinding` selects all pods in the binding's namespace — this is the canonical pattern for "namespace-default membership in vnet X" under the new model.
