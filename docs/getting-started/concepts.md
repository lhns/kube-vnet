# Concepts

This document explains the model. If you've already read the [project README](../README.md) and want the deeper "what" and "why" before touching YAML, this is the right page.

For the "why each design choice was made" rationale, see the [ADRs](../adr/README.md).

---

## The problem kube-vnet solves

In Kubernetes, by default, every pod can reach every other pod. There is no implicit isolation between namespaces, between services, or between tenants. The only mechanism for pod-level isolation in core Kubernetes is `networking.k8s.io/v1 NetworkPolicy`.

`NetworkPolicy` works, but its model is awkward for the way teams actually think about connectivity:

- It is **exception-based**. The default is allow-all; you write rules that *deny* by being more specific (since once *any* policy selects a pod, that pod becomes default-deny for the matching `policyTypes`).
- It is **selector-based**. You describe traffic in terms of `matchLabels` / `matchExpressions` on pods and namespaces. Reviewing whether *service A* can reach *service B* means tracing two sets of label selectors and the implicit OR across all matching policies.
- The default-deny baseline is non-decorative — without it, the abstraction is meaningless — but it has to be remembered and maintained per namespace.

Most teams reason about connectivity differently: *"the payments service joins the payments network, so do orders, so does its monitoring sidecar. Nothing else can reach those pods."* That's **membership-based** ("services join named networks") and **allowlist-by-construction** ("only same-network pods communicate"). This is exactly Docker Swarm's named-network primitive.

kube-vnet introduces that mental model on top of stock Kubernetes, without requiring CNI extensions. You declare a `VirtualNetwork`, services join it via labels (or via a `VirtualNetworkBinding`), and the operator emits the underlying `NetworkPolicy` set.

For the longer "why a CRD at all" treatment, see [ADR 0001](../adr/0001-virtualnetwork-as-named-network-abstraction.md).

---

## The mental model

A `VirtualNetwork` is a named group of pods that can communicate. Members of the group reach each other. Non-members (and members of other groups) cannot.

The "virtual" qualifier is deliberate: there is no separate network plane, no overlay, no tunnel. Traffic continues to flow through whatever CNI your cluster runs. The operator simply shapes the `NetworkPolicy` set so connectivity follows membership.

A pod can be a member of zero, one, or many VirtualNetworks. Membership composes additively at the pod level: a pod on networks A *and* B can reach any pod in A *or* B.

A `VirtualNetwork` is a Kubernetes resource. It lives in a "home" namespace. It can permit pods from other namespaces to join (see [`allowedNamespaces`](#cross-namespace-reach-allowednamespaces) below). It is **not** cluster-scoped; reach is a property of the network, not its identity. (See [ADR 0005](../adr/0005-namespaced-crd-with-allowed-namespaces.md) for the rationale.)

---

## Joining: the label contract

Pods declare membership via **one label per joined VirtualNetwork**. The operator inspects both label *keys* and label *values* — the value carries a [direction mode](#direction-modes-on-the-join-label) (`both`, `ingress`, `egress`, `none`).

Two label-key forms are recognized:

| Form | Used by | Example |
|---|---|---|
| **Bare** `kube-vnet/net.<vnet>` | Pods in the VirtualNetwork's home namespace | `kube-vnet/net.payments=both` |
| **Prefixed** `kube-vnet/net.<homeNS>.<vnet>` | Pods in any namespace (including the home namespace; required for foreign namespaces) | `kube-vnet/net.platform.payments=both` |

The dot separator distinguishes the two forms. A single dot after `net.` means "in this pod's namespace"; two dots means "namespace-prefixed reference."

**Long-form in the home namespace.** A pod in the vnet's home namespace can use *either* the bare or the prefixed form (or both). This makes templated workloads — e.g., a Helm chart deployed into multiple namespaces — usable with a single label key. See [ADR 0022](../adr/0022-long-form-join-label-in-home-namespace.md).

VirtualNetwork names cannot contain dots. The CRD enforces this at admission via a CEL rule (see [ADR 0017](../adr/0017-name-validation-via-cel-and-runtime-check.md)). The encoding is therefore unambiguous.

Why one label per network rather than a comma-separated list? See [ADR 0003](../adr/0003-one-label-per-virtualnetwork.md) — three concrete reasons (selector simplicity; 63-character label-value limit; matches the "one label per category" Kubernetes convention).

The full label form for cross-namespace references is documented in [ADR 0004](../adr/0004-bare-vs-namespace-prefixed-join-label.md).

---

## Direction modes on the join label

The join label *value* declares which directions a pod participates in. Recognized values:

| Value | Meaning |
|---|---|
| `both` (default) | Bidirectional. Accept ingress from peers; initiate egress to peers. |
| `ingress` | Accept-only. Accept ingress from peers; do not initiate to them. |
| `egress` | Initiate-only. Send egress to peers; do not accept from them. |
| `none` | Not a member. Equivalent to label absent. |

The legacy `"true"`, `"false"`, and empty-string aliases were dropped per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md). Use `both`/`ingress`/`egress`/`none` exclusively.

Unknown values (typos like `"bothh"`) are rejected at admission by the chart's `ValidatingAdmissionPolicy`, and on older clusters surface on the vnet's `Degraded` condition with reason `UnknownDirection`, naming the offending pods. The pod is excluded from membership; nothing is silently allowed.

### The `default-*` variants (baseline tiers only)

Baselines (`ClusterVirtualNetworkBaseline`, `VirtualNetworkBaseline`) accept four additional values: `default-both`, `default-ingress`, `default-egress`, `default-none`. The `default-` prefix marks the value **advisory** — a lower tier (a namespace baseline under the cluster baseline, or a binding/pod label under either) may override it per vnet. A *bare* value at a baseline is **enforced**: override attempts are rejected (surfaced as `OverrideRejected` on the overriding baseline) and the upstream value stays in effect. Pod-tier sources (labels, bindings) accept only the bare four; the prefix is consumed during resolution, so the stamped result on the pod is always bare. See [ADR 0031](../adr/0031-baseline-tier-resolution.md) and [the deny-all baseline section](#the-deny-all-baseline) below for how the chart presets use these.

### Traffic-flow algebra

For two members `X` and `Y` of the same vnet, traffic flows `X → Y` iff:

- `X` is initiator-capable (`both` or `egress`) **and**
- `Y` is receiver-capable (`both` or `ingress`).

| X mode | Y mode | X→Y | Y→X |
|---|---|---|---|
| both | both | yes | yes |
| both | ingress | yes | no |
| both | egress | no | yes |
| ingress | ingress | no | no |
| ingress | egress | no | yes |
| egress | egress | no | no |

### Membership policy emission

The operator emits **one ingress-only policy per (vnet, namespace) with at least one receiver-capable member**, named `kube-vnet.mem.<homeNS>.<vnet>-<8hex>` (identity per [ADR 0033](../adr/0033-canonical-fq-system-labels.md), kind-prefixed name per [ADR 0039](../adr/0039-uniform-kind-prefixed-policy-naming.md)). The selector matches the canonical FQ system label `kube-vnet.system/net.<homeNS>.<vnet>` with `value In [both, ingress]` — every member that can accept ingress. `policyTypes: [Ingress]`.

`egress`-only members produce **no self-policy** — they accept no ingress, and the operator never restricts egress (ADR 0025). They still appear in *other* members' `ingress.from` peer rules via the `In [both, egress]` selector.

`VirtualNetworkBinding`-driven members are not special-cased: the resolution controller stamps the same canonical FQ system label on selected pods (per ADR 0033), and they show up in the regular per-`(vnet, namespace)` membership policy. There is no per-binding policy.

The pre-resolution per-direction split (separate `-ingress` / `-egress` suffixed policies) and the bare-vs-prefixed dual emission were consolidated in ADRs [0021](../adr/0021-direction-modes-on-join-labels.md) (Addendum), [0022](../adr/0022-long-form-join-label-in-home-namespace.md), [0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md), and [0033](../adr/0033-canonical-fq-system-labels.md).

---

## Cross-namespace reach: `allowedNamespaces`

By default, only pods in the VirtualNetwork's **home namespace** may join. To let pods from other namespaces in, set `spec.allowedNamespaces`:

```yaml
spec:
  allowedNamespaces:
    names: [webapp, monitoring]
```

`allowedNamespaces` has three matchers, and they union:

| Field | Meaning |
|---|---|
| `all: true` | Pods in any namespace may join. |
| `names: [a, b]` | Pods in these namespaces may join. Exact match — no glob/regex; use `selector` for groups. |
| `selector` | A standard `metav1.LabelSelector`. Pods in any namespace whose labels match may join. |

The home namespace is always implicitly included. If `allowedNamespaces` is unset, only the home namespace can join.

### `allowedNamespaces` is **join eligibility, not blanket access**

This is the easiest thing to get wrong. `allowedNamespaces` controls **which namespaces' pods are *allowed to join* this network**, not "which pods are blanket-granted access to this network's members."

A pod in an `allowedNamespaces`-permitted namespace **must still add the join label** (or be selected by a `VirtualNetworkBinding` in that namespace) to become a member. Pods that don't carry the label and aren't bound get nothing.

The operator enforces this on both sides:

- **Discovery side**: the pod loop only considers a pod a member if it carries the appropriate join-label key (or matches a binding's selector). The `permits()` check on `allowedNamespaces` only runs for pods that already qualify.
- **NetworkPolicy side**: the generated `from` and `to` peer rules use `podSelector: { matchExpressions: [{ key: <join-key>, operator: In, values: [...] }] }`. Even at the policy layer, the only pods granted access are those that match the per-direction selector.

For the full treatment, see [ADR 0005 § Join eligibility, not blanket access](../adr/0005-namespaced-crd-with-allowed-namespaces.md).

### Why no glob patterns in `Names`

`Names` matches namespace names exactly. Globs like `payments-*` are deliberately unsupported because:

- Glob vs regex vs prefix is ambiguous and would have to be documented.
- Label selectors already cover "group of namespaces" cleanly.
- It's not idiomatic in Kubernetes APIs (cert-manager, Cilium, Istio do not accept globs in name lists).

If you want prefix-style matching, label your namespaces and use `selector`.

---

## `VirtualNetworkBinding`: the no-label alternative

Some pods cannot have labels added to them: their template comes from an upstream Helm chart you don't want to fork, or from another operator that re-templates them on every reconcile. For those, kube-vnet provides a `VirtualNetworkBinding` (short names `vnb`, `vnbs`) — a namespaced CRD that selects pods *in its own namespace* and attaches them to a target vnet without writing to the pods.

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
  direction: both        # both | ingress | egress | none
  podSelector:
    matchLabels:
      app: thirdparty-billing-agent
```

Behavior:

- The selector is **scoped to the binding's own namespace**. There are no cross-namespace bindings.
- The target vnet's `spec.allowedNamespaces` is enforced. A binding in a non-permitted namespace surfaces `Ready=False, Reason=NamespaceNotAllowed`.
- A binding in a `kube-vnet/disabled` (or operator-excluded) namespace is inert. The binding's status is `Ready=False, Reason=NamespaceExcluded`.
- No per-binding policy is emitted. The resolution controller stamps the canonical FQ system label `kube-vnet.system/net.<homeNS>.<vnet>` on selected pods, and they are covered by the regular per-`(vnet, namespace)` membership policy (per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)).

Bindings are an escape hatch — the join label is the recommended primary mechanism. See [ADR 0026](../adr/0026-virtualnetworkbinding-crd.md).

---

## The deny-all baseline

Without baseline isolation, `allowedNamespaces` and the membership rules above would be decorative. Kubernetes' default is allow-all: a pod with no `NetworkPolicy` selecting it can reach any other pod.

Per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) and [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md), kube-vnet installs a single uniform baseline in every managed namespace: deny-all ingress (`policyTypes: [Ingress]`, no allow rules) selecting every pod. Vnet members get additive allows from their membership policies; pods that aren't members of anything get nothing through.

**Egress is unrestricted by the baseline.** Membership policies are ingress-only; generic egress (DNS, the apiserver, the public internet, other namespaces) is not restricted by kube-vnet. If you need per-workload egress restriction, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` — see [`recipes.md`](../guides/recipes.md).

The baseline `NetworkPolicy` is named `kube-vnet.base` (per [ADR 0039](../adr/0039-uniform-kind-prefixed-policy-naming.md)) and labeled `kube-vnet.system/managed-by=kube-vnet, kube-vnet.system/role=baseline`.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.base
  namespace: <ns>
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/role: baseline
spec:
  podSelector: {}            # selects every pod in the namespace
  policyTypes: [Ingress]
  # no `ingress:` rules — deny-all
```

See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) and [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md). The previous `--elide-baseline-for` flag (which added `NotIn` matchExpressions to skip the baseline for cluster-receiver pods) was removed: it had no observable effect on connectivity because NetworkPolicy union semantics already make the baseline's deny-all redundant for any pod selected by a membership policy.

### Operator default vnet memberships

A singleton `ClusterVirtualNetworkBaseline` named `default` declares membership every pod inherits, with per-vnet override-permission encoded in the eight-value `Direction` enum (bare = enforced, `default-*` = override-permitted by lower tiers). The chart seeds this CR from `operator.clusterBaseline.{create, ingressIsolationLevel, memberships}` — pick a preset (`pod` / `namespace` / `cluster`) or supply an explicit memberships map. Per-namespace overrides go in a `VirtualNetworkBaseline` named `default` in the namespace; per-pod overrides go in a `VirtualNetworkBinding` (must select specific pods) or via the `kube-vnet/net.<vnet>=<dir>` label. Conflicts within a tier (or across siblings at the pod tier) resolve via intersection (fail-closed). See ADR 0031.

### Baseline ownership

The baseline lifecycle is owned by the **`NamespaceReconciler`**, which watches namespaces and applies the deny-all baseline to every managed namespace. The `VirtualNetworkReconciler` only writes membership policies; it never touches the baseline.

### Disabling the operator for a namespace

The annotation `kube-vnet/disabled=true` is a separate, orthogonal switch. When set, the operator does nothing in that namespace: no baseline, no membership policies, no system vnet, no eligibility as a peer, no honoring bindings. The operator-level flag `--disabled-namespaces` (default: `kube-system`, `kube-public`, `kube-node-lease`, plus the operator's own namespace added implicitly) has the same effect at the cluster level.

See [ADR 0006](../adr/0006-baseline-default-deny-and-single-opt-out.md) (superseded by ADR 0023 for the baseline-control half, then by ADR 0030 for the shape), [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md), and [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md).

---

## How the policies compose

The whole policy set is **two layers stacked**:

1. **Baseline (deny-all floor)** — one `NetworkPolicy` named `kube-vnet.base` per managed namespace. `policyTypes: [Ingress]`, zero allow rules, `PodSelector: {}` (selects every pod). Egress is never restricted by the baseline.
2. **Membership policies (additive allows)** — one per `(vnet, namespace)`. Select pods that carry `kube-vnet.system/net.<homeNS>.<vnet>` in `[both, ingress]` and add `from:` rules naming peers across all member-bearing namespaces. They only ever *add* to allowed traffic.

For "always-open" patterns like the cluster system vnet (every pod is on `cluster=both`), there's no third layer needed: the cluster membership policy's `from:` rules name every cluster-vnet sender, which under `cluster=default-both` resolves to ≈ every pod. The baseline's deny-all is overridden by the membership's allows via NetworkPolicy union. (Earlier versions had a `--elide-baseline-for` flag here; removed in [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md) — it had no observable effect.)

### The composition rule, in one sentence

NetworkPolicy is additive: a pod's effective allowed ingress is the **union** of `from:` rules across every policy that selects it. If no policy selects it, Kubernetes' default-allow applies. If the baseline selects it but no membership policy does, deny-all wins (the baseline has zero allow rules).

### Picture it

```
┌─────────────────────────── namespace: webapp ─────────────────────────────┐
│                                                                           │
│   pod orders [system: net.payments=both, net.cluster=egress]              │
│   selected by: ┌─ baseline (selects every pod)                       ┐    │
│                ├─ kube-vnet.mem.platform.payments-<hash> (membership: payments)   ┤    │
│                └─ (no cluster membership policy: pod is egress-only) ┘    │
│   effective:   deny-all  ⊕  allow-from-payments-peers  =  payments-only   │
│                                                                           │
│   pod metrics [system: net.cluster=both]                                  │
│   selected by: ┌─ baseline (selects every pod)                       ┐    │
│                └─ kube-vnet.mem.cluster-<hash> (membership: cluster)     ┘    │
│   effective:   deny-all  ⊕  allow-from-cluster-peers  =  allow-from-cluster│
│                                                                           │
│   pod cron-x [no system labels (no memberships, or unresolved)]           │
│   selected by: └─ baseline (selects every pod)                       ┘    │
│   effective:   deny-all (no allows added)                                 │
└───────────────────────────────────────────────────────────────────────────┘
```

`⊕` is policy union: each policy contributes its `from:` rules, the apiserver merges them, and that's what the CNI enforces.

### The same picture as a table

| Pod's system labels | Baseline selects? | Membership policies selecting | Effective ingress |
|---|---|---|---|
| (none — unresolved or no memberships) | yes | — | denied (deny-all only) |
| `net.payments=both` | yes | `kube-vnet.mem.platform.payments-<hash>` | from same-payments peers only |
| `net.cluster=both` | yes | `kube-vnet.mem.cluster-<hash>` | from any cluster peer (≈ allow-from-anywhere when cluster is universal) |
| `net.payments=both, net.cluster=both` | yes | both `payments` and `cluster` membership policies | union: from payments peers OR cluster peers (≈ allow-from-anywhere) |

Cross-namespace ingress always requires the receiving pod to be a vnet member whose membership policy allows the sender; the deny-all baseline blocks anything else.

### Unresolved pods get the deny floor (fail-closed)

A pod that hasn't yet been processed by `ResolutionReconciler` carries no `kube-vnet.system/*` labels. It doesn't match any membership policy's selector, so the baseline selects it (the baseline selects every pod) → deny-all. This is the safety net during the resolution race window. Once resolution stamps the system labels (typically within milliseconds of pod creation), the pod transitions to whatever its memberships dictate. See [`architecture.md`](../internals/architecture.md) for how the resolution controller decides what to stamp.

---

## The generated NetworkPolicy: per (vnet, namespace)

For each VirtualNetwork with at least one receiver-capable member, the operator generates **one membership `NetworkPolicy` per (vnet, namespace)**. Bindings do not produce additional policies — binding-targeted pods are stamped with the same canonical FQ system label and are covered by the regular membership policy (per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)).

Naming: `kube-vnet.mem.<homeNS>.<vnet>-<8hex>` uniformly (`kube-vnet.mem.cluster-<8hex>` for the cluster system vnet). The 8-hex suffix is a SHA-256-based identity hash that disambiguates against name collisions. The truncate-and-hash overflow handler still applies if the rendered name exceeds Kubernetes' 253-character resource-name limit. See [ADR 0011](../adr/0011-policy-naming-and-truncation.md) (refined by [ADR 0033](../adr/0033-canonical-fq-system-labels.md)) and [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md).

Labels on every operator-managed `NetworkPolicy`:

- `kube-vnet.system/managed-by=kube-vnet` — claims operator ownership. Used by drift correction and cleanup.
- `kube-vnet.system/network=<homeNS>.<vnet>` — identifies which VirtualNetwork owns the policy. Used for cleanup, including cross-namespace.
- `kube-vnet.system/role=membership` (membership policies) or `=baseline` (baseline policies).

Owner references: only set when the policy is in the same namespace as the VirtualNetwork. Kubernetes does not support cross-namespace owner references. For policies in foreign namespaces, the operator manages cleanup via the `kube-vnet.system/network` label — see [ADR 0010](../adr/0010-cross-namespace-cleanup-via-network-label.md).

This is why the operator can't do its job from a single cluster-scoped policy: stock `NetworkPolicy` is namespace-local. Each side needs its own policy. (For the future where this changes, see [ADR 0019](../adr/0019-baseline-durability.md) on `AdminNetworkPolicy`.)

---

## Drift correction

The operator watches every `NetworkPolicy` carrying `kube-vnet.system/managed-by=kube-vnet`. If one is edited or deleted out-of-band:

- An update event fires, the policy's `kube-vnet.system/network` label maps it back to the owning VirtualNetwork, and the reconciler re-applies the desired spec via server-side apply with field manager `kube-vnet`.
- A delete event does the same: the reconciler re-creates the missing policy.
- On re-creation specifically (i.e. the policy was absent immediately before the apply), a `Warning PolicyRestored` Event is emitted on the owning VirtualNetwork so the deletion-and-restore cycle is visible in `kubectl describe vnet`.

Server-side apply is used with `client.ForceOwnership`, so the operator reliably reclaims field ownership on its own resources. See [ADR 0009](../adr/0009-server-side-apply-with-field-manager.md) and [ADR 0019](../adr/0019-baseline-durability.md).

**What drift correction does *not* do:** it can't prevent the deletion in the first place. There is a sub-second-to-a-few-seconds window where the policy is gone and traffic that the policy would have denied is allowed. Drift correction is a best-effort defense against accidental deletion, unaware tooling, and most non-malicious cases. For hard-guarantee namespace-RBAC-resistant deny rules, the proper Kubernetes tool is `AdminNetworkPolicy` — tracked in ADR 0019 as the future direction.

---

## Status conditions: what the operator tells you

Each VirtualNetwork carries two conditions in `status.conditions`:

- **`Ready`** — true when the desired NetworkPolicy set has been applied. False when something is preventing reconciliation (apply error, invalid name, home namespace excluded).
- **`Degraded`** — true when some subset of the desired state can't be honored (a labeled pod is in a non-permitted namespace, an unknown direction value, a cross-source resolution conflict between a binding and a label or between two bindings on the same pod, or a name collision with a user-managed NetworkPolicy). See `ResolutionConflict` in the reasons taxonomy.

Each `VirtualNetworkBinding` similarly carries a `Ready` condition with reasons `PodsAttached`, `NoPodsMatch`, `VirtualNetworkNotFound`, `NamespaceNotAllowed`, `NamespaceExcluded`, `UnknownDirection`, or `InvalidSelector`.

Both conditions follow the standard `metav1.Condition` shape: `type`, `status`, `reason`, `message`, `lastTransitionTime`. Tools that consume this pattern (`kubectl wait --for=condition=Ready`, dashboards, event aggregators) work out of the box.

Transitions also emit Kubernetes Events. See [ADR 0012](../adr/0012-status-conditions-ready-and-degraded.md) and [ADR 0016](../adr/0016-emit-events-on-condition-transitions.md), and the full reason taxonomy in [`reference/api.md`](../reference/api.md).

---

## Where to go from here

- **Try it**: [`install.md`](install.md) → [`first-vnet.md`](first-vnet.md) (hands-on walkthrough) → [`recipes.md`](../guides/recipes.md).
- **Look up a field or value**: [`reference/`](../reference/).
- **Run it in production**: [`operations.md`](../guides/operations.md), [`security.md`](../security/security.md).
- **Understand the policies the operator creates on its own**: [`auto-allow.md`](../guides/auto-allow.md).
- **Debug it**: [`troubleshooting.md`](../guides/troubleshooting.md).
- **Read the design rationale**: [`adr/`](../adr/README.md).
