# 0031 — Baseline-tier resolution: replace bindings/CVNB with explicit defaults vs bindings

> **Amendment (ADR 0043, 2026-07-09)**: `virtualNetworkRef.namespace` is now optional and, when set, honored rather than ignored. Omitting it is the recommended form for the system vnets. See [ADR 0043](0043-virtualnetworkref-namespace-inferred-or-honored.md).

Status: Accepted

Date: 2026-05-05

Partially supersedes: [ADR 0030](0030-unified-vnet-membership-with-resolution.md) — the resolution-lattice section.

Updates: [ADR 0026](0026-virtualnetworkbinding-crd.md) — VirtualNetworkBinding now requires a non-empty `podSelector`.

## Context

ADR 0030 introduced a four-scope resolution lattice: `OperatorDefault < ClusterVirtualNetworkBinding < VirtualNetworkBinding < PodLabel`. Higher specificity (closer to the pod) wins. Two recurring problems with that shape, found while documenting and onboarding:

1. **CRD names lie about the role.** A `ClusterVirtualNetworkBinding` with empty selectors is not binding any specific workload; it is setting a cluster-wide *default*. A `VirtualNetworkBinding` with empty `podSelector` is a namespace-wide default. Reading the YAML, you cannot tell whether you are looking at a workload-attachment or a tier-default. In practice ~95% of CVNBs in real deployments are written with `namespaceSelector: {}` and `podSelector: {}`, so the "binding" framing is the wrong primary metaphor.

2. **No authority-vs-specificity distinction.** Today a namespace-admin's `VirtualNetworkBinding` silently overrides a cluster-admin's `ClusterVirtualNetworkBinding`. SIG-network's `AdminNetworkPolicy` / `BaselineAdminNetworkPolicy` exists precisely so cluster-admins can lock things down that namespace-admins cannot widen — kube-vnet has no equivalent guardrail. The trust model is "anyone with namespace-binding rights can rebroadcast their pods onto any vnet they can name." For multi-tenant clusters this is not a credible policy layer.

A third nuisance falls out of the same shape: the within-scope conflict tiebreaker (alphabetical by source name) is arbitrary. Two equally-valid bindings disagreeing on direction produce a deterministic but semantically meaningless winner.

## Decision

Mirror the SIG-network ANP/BANP precedent and the Gateway API "Inherited Policy" pattern. Split the API surface into **baselines** (tier-defaults, override-permission per-vnet) and **bindings** (workload-specific attachment only). Replace the alphabetical tiebreaker with **fail-closed intersection**.

### Resolution lattice

```
ClusterVirtualNetworkBaseline (cluster-scoped singleton, name=default)
   ↓
VirtualNetworkBaseline        (namespace-scoped singleton per NS, name=default)
   ↓
VirtualNetworkBinding         (namespace-scoped, non-empty podSelector required)
   ↓
Pod label                     (kube-vnet/net.<vnet>=<dir>)
```

Each tier can add, override (when permitted), or opt-out (`direction=none`) a `(vnet, direction)` pair for a pod. Override-permission is per-vnet and explicit, encoded in the direction value at the upstream tier (see below).

The four scopes from ADR 0030 (`ScopeOperatorDefault`, `ScopeClusterBinding`, `ScopeNamespaceBinding`, `ScopePodLabel`) are replaced by four new scopes (`ScopeClusterBaseline`, `ScopeNamespaceBaseline`, `ScopeBinding`, `ScopePodLabel`). The operator-flag `--default-memberships` is folded into a chart-seeded `ClusterVirtualNetworkBaseline` and deprecated as a flag.

### Direction enum: bare = enforced, `default-*` = override-able

The previous four-value enum (`both`, `ingress`, `egress`, `none`) gains four prefixed siblings:

| Value           | Meaning                                                          | Tiers that may set it          |
|-----------------|------------------------------------------------------------------|--------------------------------|
| `both`          | Effective both, **no override permitted** by lower tiers         | Baselines only (enforced)      |
| `ingress`       | Effective ingress, no override                                   | Baselines only (enforced)      |
| `egress`        | Effective egress, no override                                    | Baselines only (enforced)      |
| `none`          | Effective none, no override (hard opt-out)                       | All tiers                      |
| `default-both`  | Effective both, **lower tiers may override**                     | Baselines only                 |
| `default-ingress` | Effective ingress, override permitted                          | Baselines only                 |
| `default-egress`  | Effective egress, override permitted                           | Baselines only                 |
| `default-none`    | Effective none, override permitted                             | Baselines only                 |

At the **pod tier** (label or `VirtualNetworkBinding`), only the bare four are valid. The `default-*` family signals "this is a default for downstream tiers" — there is nothing downstream of pod-tier values, so the prefix is meaningless and would be confusing. CRD CEL on `VirtualNetworkBinding.spec.direction` and runtime parsing of the pod label both reject the prefixed forms.

The final emitted direction (the value stamped onto the pod as `kube-vnet.system/net.<vnet>=<dir>`) is always one of the bare four. The `default-` prefix is consumed during resolution to compute override-permission.

### Conflict resolution: intersection (fail-closed)

When two sources at the same tier (or sibling tiers — pod-label and binding) disagree on direction for the same vnet, the effective direction is their **intersection**:

| a \ b   | both    | ingress | egress  | none |
|---------|---------|---------|---------|------|
| both    | both    | ingress | egress  | none |
| ingress | ingress | ingress | none    | none |
| egress  | egress  | none    | egress  | none |
| none    | none    | none    | none    | none |

Symmetric. Any participant of `none` zeroes the result. Differing single-direction values (`ingress` vs `egress`) collapse to `none`. The conflict is still reported through ADR 0030's existing surfaces — `ResolutionResult.Conflicts`, the `kube-vnet.system/conflict.<vnet>` annotation on the pod, and the `kube_vnet_resolution_conflicts_total` metric — so the user sees what to fix; intersection is just the safe behaviour while they fix it.

This replaces the alphabetical-by-source tiebreaker from ADR 0030. Intersection is deterministic without being arbitrary; it has a security-grounded rationale (fail-closed during conflict windows); and it is consistent with how every other layer of the k8s NetworkPolicy stack (NP, ANP, PSA) handles ambiguity.

### Singleton baselines

Both `ClusterVirtualNetworkBaseline` and `VirtualNetworkBaseline` are singletons:

- One `ClusterVirtualNetworkBaseline` per cluster.
- One `VirtualNetworkBaseline` per namespace.
- Both must be named `default`.

Enforcement uses CRD CEL (`self.metadata.name == 'default'`), the same pattern `BaselineAdminNetworkPolicy` uses. Combined with the apiserver's existing per-name uniqueness guarantee (one `name+kind+cluster` triple, one `name+kind+namespace` triple), the singleton property is automatic.

### Why singletons (and not multiple priority-ordered baselines)

ANP allows multiple priority-ordered cluster-scoped policies; BANP is the singleton floor. We chose the BANP-style singleton at both tiers for two reasons:

1. **Cognitive simplicity**: a cluster-admin defending the operator's posture wants to see `kubectl get clustervirtualnetworkbaseline default -o yaml` and read one document. Priority ordering across multiple cluster-scoped CRs would make the effective policy harder to reason about, and our use case (a small handful of system vnets, plus user-defined vnets joined by selectors via Bindings) does not need it.
2. **Selector richness covers the "multiple policies" case**: a single baseline holds a list of `(vnetRef, direction)` memberships, each able to scope further via the bindings tier. The expressivity ANP gets from priority+selectors is folded into baseline-list-of-memberships + workload-specific bindings.

If a future use case demonstrates the need for multiple priority-ordered cluster baselines, we can extend the model without breaking existing single-baseline users.

### Examples

#### Cluster baseline (the chart seeds this from `isolationLevel`)

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: ClusterVirtualNetworkBaseline
metadata:
  name: default
spec:
  memberships:
    - virtualNetworkRef: { name: namespace, namespace: kube-vnet-system }
      direction: default-both          # NS baselines may override
    - virtualNetworkRef: { name: cluster,   namespace: kube-vnet-system }
      direction: default-egress        # NS baselines may override
```

#### Namespace baseline

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBaseline
metadata:
  name: default
  namespace: webapp
spec:
  memberships:
    - virtualNetworkRef: { name: cluster, namespace: kube-vnet-system }
      direction: default-both          # this NS opts into cluster ingress; bindings/labels may further narrow
```

#### Workload binding (selector required)

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBinding
metadata:
  name: payments-thirdparty
  namespace: webapp
spec:
  virtualNetworkRef: { name: payments, namespace: platform }
  podSelector:
    matchLabels: { app: thirdparty-billing-agent }
  direction: both
```

#### Hard pin from cluster (cannot be overridden)

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: ClusterVirtualNetworkBaseline
metadata:
  name: default
spec:
  memberships:
    - virtualNetworkRef: { name: payments, namespace: platform }
      direction: none                   # bare → no NS baseline, binding, or label can re-add this vnet
```

A namespace-admin attempting to override this with a `VirtualNetworkBaseline` referencing the same vnet at any direction has the override **rejected**. The namespace baseline's `Status.Conditions` carries a `Reason=OverrideRejected` entry naming the offending vnet; the effective direction remains `none`.

## Consequences

### Migration

The chart seeds a `ClusterVirtualNetworkBaseline` from `operator.clusterBaseline` (new). When `create: true`, exactly one of `ingressIsolationLevel` or `memberships` must be set; both → `helm install` fails (XOR), neither → fails with a "no default" message. Three preset values map to three baselines:

| `ingressIsolationLevel` | Seeded `ClusterVirtualNetworkBaseline.spec.memberships`                                     |
|-------------------------|----------------------------------------------------------------------------------------------|
| `pod`                   | `[{namespace, default-egress}, {cluster, default-egress}]` — strict; ingress only via explicit binding/label |
| `namespace`             | `[{namespace, default-both}, {cluster, default-egress}]` — same-NS reachable, cross-NS egress-only           |
| `cluster`               | `[{namespace, default-both}, {cluster, default-both}]`   — no isolation                                       |

For configurations the presets don't cover, set `operator.clusterBaseline.memberships` instead — a map of `<vnet-key>: <direction>` where the key is a bare system-vnet name (resolves to `{name: <key>, namespace: {{ .Release.Namespace }}}`) or `<namespace>.<name>` for user vnets.

The `--default-memberships` operator flag (and `operator.defaultMemberships` chart value) and the `ClusterVirtualNetworkBinding` CRD are removed. Pre-1.0 cleanup; no deprecation window.

`VirtualNetworkBinding` with empty `podSelector` is rejected at admission going forward (CEL validation). Existing CRs continue to read-back; any `kubectl edit` requires migrating the empty-selector case to a `VirtualNetworkBaseline`.

### Trust gradient

The new lattice gives cluster-admins and namespace-admins explicit, asymmetric authority:

- **Cluster-admin authors** `ClusterVirtualNetworkBaseline`. Bare directions enforce; `default-*` defers to lower tiers.
- **Namespace-admin authors** `VirtualNetworkBaseline` (NS-singleton) and `VirtualNetworkBinding` (workload). They can override `default-*` cluster-baseline entries within their NS; they cannot override bare ones.
- **Pod author** sets labels. They can override `default-*` namespace-baseline entries for their own pod; they cannot override anything bare upstream.

This is the authority-respects-specificity model the resolution lattice was always conceptually trying to express — now made explicit instead of implicit.

### Removed surface

- The `kube-vnet/inherit=false`-shaped per-pod opt-out label, considered briefly during planning, is unnecessary under this model. Per-vnet `kube-vnet/net.<vnet>=none` remains the per-pod escape hatch, and the cluster-admin's `default-*` choice controls whether it can take effect.
- The within-scope alphabetical tiebreaker is removed.
- `ScopeOperatorDefault` is removed (the chart-seeded `ClusterVirtualNetworkBaseline` plays its role).

## Addenda

### Vnet-key form in baselines

Baseline `memberships` reference vnets by `(name, namespace)`. The chart's `operator.clusterBaseline.memberships` map shorthand admits two key forms — bare (`namespace`, `cluster`) and dotted (`<namespace>.<name>`) — that mirror the bare/prefixed pod-label convention from earlier ADRs, but with subtly different semantics at the baseline tier:

- **Pod labels** admit both forms: bare = "this vnet in my own namespace"; prefixed = explicit cross-NS reference. The form is fully resolved at the time the label is read.
- **Baselines** also admit both forms, but a bare key is only meaningful for the system vnets (`namespace`, `cluster`). The reserved-name VAP from the ADR-0030 follow-up guarantees no user vnet can collide with those names, so the dispatch is unambiguous in practice. A bare-keyed entry in a baseline names a *scope-relative* vnet — it expands to the matching bare pod label at resolution time, which means a single baseline entry produces a per-pod-namespace effect for the per-NS `namespace` system vnet. Conceptually a partially-applied function: the baseline carries the unparameterized membership, the pod-resolution step parameterizes it.
- Specifying the `cluster` system vnet with a `kube-vnet-system.cluster` (release-namespace-prefixed) form works and is now *verified* against the real CR, but remains discouraged because it couples user-facing config to the operator's release namespace. **Omit `namespace` instead** (ADR 0043). Naming any other namespace resolves to a vnet that does not exist and the membership is dropped with a `VirtualNetworkNotJoinable` Event — the `namespace` system vnet in particular lives in every *managed* namespace, never in the release namespace.

### Validation limits on baseline references

Baseline references cannot be validated for vnet existence at admission time. CRD CEL only sees the baseline document being admitted, never the cluster's set of `VirtualNetwork` CRs; a webhook that read other resources would race with vnet creation/deletion (vnet exists at admission, gets deleted moments later — false-positive validation that doesn't reflect reality). A reference to a non-existent vnet is accepted at admission and silently becomes a no-op at pod-resolution time: the resolver simply doesn't emit a `kube-vnet.system/net.<vnet>` label for that membership.

Bare-keyed entries are even more dynamic — the effective vnet differs per pod's namespace, so the same baseline entry could be valid for some pods and a no-op for others. Validation surfaces at pod-resolution time (status conditions, the `kube_vnet_resolution_conflicts_total` metric, missing system labels on pods that should have inherited them); operators wanting tighter feedback should `kubectl get vnet -A` to confirm references resolve.

## Out of scope

- Multiple priority-ordered cluster baselines (single singleton suffices for current use cases; revisit if pressure emerges).
- A separate ANP `Pass` action analogue (the bare/`default-*` split covers the same expressivity).
- Replacing `--disabled-namespaces` with a CR (orthogonal).
- Cross-cluster federation of baselines.
