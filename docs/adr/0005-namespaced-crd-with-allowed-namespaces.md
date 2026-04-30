# 0005 — Namespaced CRD with allowedNamespaces

Status: Accepted (supersedes the design doc's `spec.extent` proposal)

## Context

The design doc originally specified `spec.extent` as an enum (`Namespace` | `Cluster`) describing the maximum reach of a VirtualNetwork. Two variants implied two different policy-generation strategies in one type and left "should `extent: Cluster` VirtualNetworks live in a home namespace?" as an open question.

Three alternatives were considered:

1. Single namespaced CRD with `spec.extent` (the original design).
2. Two CRDs — `VirtualNetwork` (namespaced) and `ClusterVirtualNetwork` (cluster-scoped). Idiomatic in Kubernetes (cf. Role/ClusterRole, Issuer/ClusterIssuer, Cilium NetworkPolicy/ClusterNetworkPolicy).
3. Single namespaced CRD with `spec.allowedNamespaces` controlling reach as a property of the network, not its identity.

## Decision

Adopt option 3. Every `VirtualNetwork` is namespaced and belongs to one application namespace that owns it. Reach is controlled by `spec.allowedNamespaces`, a structured matcher:

```go
type NamespaceSelector struct {
    All      bool                  // wildcard: any namespace
    Names    []string              // explicit names; exact match only
    Selector *metav1.LabelSelector // label-based
}
```

The home namespace is always implicitly allowed. If `allowedNamespaces` is unset, only the home namespace can join.

**Wildcard semantics — explicit:**

- `{ all: true }` is the only wildcard form.
- `{ names: [...] }` matches exactly. Glob patterns (`payments-*`) are deliberately not supported — they're ambiguous (glob vs regex vs prefix), redundant with label selectors, and not idiomatic in Kubernetes APIs (cert-manager, Cilium, Istio do not accept globs in name lists). Users wanting prefix matching should label their namespaces and use `selector`.
- Combining `Names` and `Selector` unions; combining either with `All` is meaningless (`All` wins).

**Join eligibility, not blanket access:**

`allowedNamespaces` answers "which namespaces' pods are *allowed to join* this network?", **not** "which pods are granted access to this network's members?". A pod in a permitted namespace still has to add the prefixed join label `kube-vnet/net.<homeNS>.<vnet>=true` to become a member; pods in the namespace that don't carry the label get nothing. The reconciler enforces this on the discovery side (only labeled pods become members), and the generated `NetworkPolicy` peer rules use `podSelector: { matchExpressions: [{ key: <join-key>, operator: Exists }] }` so even at the policy layer, the only pods granted access are those carrying the join key.

## Consequences

- **Pro**: One policy-generation strategy, not two. Simpler reconciler.
- **Pro**: Every VirtualNetwork has a clear ownership anchor (a namespace), which gives RBAC and lifecycle a natural home.
- **Pro**: Future extension to cross-cluster reach is additive: `allowedNamespaces` can grow into `allowedPeers` containing namespace and cluster matchers without breaking existing manifests.
- **Pro**: Removes the design doc's "home namespace for cluster-extent vnets" open question.
- **Con**: Breaking change vs the originally-published `spec.extent` API. Acceptable: `v1alpha1` carries no compatibility promise, and this lands the same day as the original publication.
- **Note**: The design doc (`docs/kube-vnet-design.md`) still uses `extent`; this ADR is the source of truth for the implemented model.
