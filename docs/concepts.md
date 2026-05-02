# Concepts

This document explains the model. If you've already read the [project README](../README.md) and want the deeper "what" and "why" before touching YAML, this is the right page.

For the "why each design choice was made" rationale, see the [ADRs](adr/README.md).

---

## The problem kube-vnet solves

In Kubernetes, by default, every pod can reach every other pod. There is no implicit isolation between namespaces, between services, or between tenants. The only mechanism for pod-level isolation in core Kubernetes is `networking.k8s.io/v1 NetworkPolicy`.

`NetworkPolicy` works, but its model is awkward for the way teams actually think about connectivity:

- It is **exception-based**. The default is allow-all; you write rules that *deny* by being more specific (since once *any* policy selects a pod, that pod becomes default-deny for the matching `policyTypes`).
- It is **selector-based**. You describe traffic in terms of `matchLabels` / `matchExpressions` on pods and namespaces. Reviewing whether *service A* can reach *service B* means tracing two sets of label selectors and the implicit OR across all matching policies.
- The default-deny baseline is non-decorative — without it, the abstraction is meaningless — but it has to be remembered and maintained per namespace.

Most teams reason about connectivity differently: *"the payments service joins the payments network, so do orders, so does its monitoring sidecar. Nothing else can reach those pods."* That's **membership-based** ("services join named networks") and **allowlist-by-construction** ("only same-network pods communicate"). This is exactly Docker Swarm's named-network primitive.

kube-vnet introduces that mental model on top of stock Kubernetes, without requiring CNI extensions. You declare a `VirtualNetwork`, services join it via labels (or via a `VirtualNetworkBinding`), and the operator emits the underlying `NetworkPolicy` set.

For the longer "why a CRD at all" treatment, see [ADR 0001](adr/0001-virtualnetwork-as-named-network-abstraction.md).

---

## The mental model

A `VirtualNetwork` is a named group of pods that can communicate. Members of the group reach each other. Non-members (and members of other groups) cannot.

The "virtual" qualifier is deliberate: there is no separate network plane, no overlay, no tunnel. Traffic continues to flow through whatever CNI your cluster runs. The operator simply shapes the `NetworkPolicy` set so connectivity follows membership.

A pod can be a member of zero, one, or many VirtualNetworks. Membership composes additively at the pod level: a pod on networks A *and* B can reach any pod in A *or* B.

A `VirtualNetwork` is a Kubernetes resource. It lives in a "home" namespace. It can permit pods from other namespaces to join (see [`allowedNamespaces`](#cross-namespace-reach-allowednamespaces) below). It is **not** cluster-scoped; reach is a property of the network, not its identity. (See [ADR 0005](adr/0005-namespaced-crd-with-allowed-namespaces.md) for the rationale.)

---

## Joining: the label contract

Pods declare membership via **one label per joined VirtualNetwork**. The operator inspects both label *keys* and label *values* — the value carries a [direction mode](#direction-modes-on-the-join-label) (`both`, `ingress`, `egress`, `none`).

Two label-key forms are recognized:

| Form | Used by | Example |
|---|---|---|
| **Bare** `kube-vnet/net.<vnet>` | Pods in the VirtualNetwork's home namespace | `kube-vnet/net.payments=both` |
| **Prefixed** `kube-vnet/net.<homeNS>.<vnet>` | Pods in any namespace (including the home namespace; required for foreign namespaces) | `kube-vnet/net.platform.payments=both` |

The dot separator distinguishes the two forms. A single dot after `net.` means "in this pod's namespace"; two dots means "namespace-prefixed reference."

**Long-form in the home namespace.** A pod in the vnet's home namespace can use *either* the bare or the prefixed form (or both). This makes templated workloads — e.g., a Helm chart deployed into multiple namespaces — usable with a single label key. See [ADR 0022](adr/0022-long-form-join-label-in-home-namespace.md).

VirtualNetwork names cannot contain dots. The CRD enforces this at admission via a CEL rule (see [ADR 0017](adr/0017-name-validation-via-cel-and-runtime-check.md)). The encoding is therefore unambiguous.

Why one label per network rather than a comma-separated list? See [ADR 0003](adr/0003-one-label-per-virtualnetwork.md) — three concrete reasons (selector simplicity; 63-character label-value limit; matches the "one label per category" Kubernetes convention).

The full label form for cross-namespace references is documented in [ADR 0004](adr/0004-bare-vs-namespace-prefixed-join-label.md).

---

## Direction modes on the join label

The join label *value* declares which directions a pod participates in. Recognized values:

| Value | Meaning |
|---|---|
| `both` (default) | Bidirectional. Accept ingress from peers; initiate egress to peers. |
| `ingress` | Accept-only. Accept ingress from peers; do not initiate to them. |
| `egress` | Initiate-only. Send egress to peers; do not accept from them. |
| `none` | Not a member. Equivalent to label absent. |

For backward compatibility with older manifests, two legacy values are accepted:

| Legacy | Maps to |
|---|---|
| `"true"` | `both` |
| `"false"` | `none` |

Unknown values (typos like `"bothh"`) surface on the vnet's `Degraded` condition with reason `UnknownDirection`, naming the offending pods. The pod is excluded from membership; nothing is silently allowed.

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

### Per-direction policies

The operator emits **up to one policy per direction class with at least one member** in each member-bearing namespace:

- `kube-vnet-<vnet>-<ns>` — bidirectional members. Selects `value In [true, both]`. `policyTypes: [Ingress, Egress]`. The unsuffixed name preserves the legacy v1alpha1 naming for the common case.
- `kube-vnet-<vnet>-<ns>-ingress` — ingress-only members. Selects `value In [ingress]`. `policyTypes: [Ingress]`.
- `kube-vnet-<vnet>-<ns>-egress` — egress-only members. Selects `value In [egress]`. `policyTypes: [Egress]`.

In the home namespace, each direction class can produce two policies — one for the bare form and one (suffixed `-prefixed`) for the prefixed form — when both forms are in use. See [ADR 0021](adr/0021-direction-modes-on-join-labels.md) and [ADR 0022](adr/0022-long-form-join-label-in-home-namespace.md).

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

For the full treatment, see [ADR 0005 § Join eligibility, not blanket access](adr/0005-namespaced-crd-with-allowed-namespaces.md).

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
- The generator emits one membership policy per binding, named `kube-vnet-<vnet>-b-<binding>`, labeled `kube-vnet/binding=<binding>` for traceability.

Bindings are an escape hatch — the join label is the recommended primary mechanism. See [ADR 0026](adr/0026-virtualnetworkbinding-crd.md).

---

## The ingress-isolation baseline

Without baseline isolation, `allowedNamespaces` and the membership rules above would be decorative. Kubernetes' default is allow-all: a pod with no `NetworkPolicy` selecting it can reach any other pod.

kube-vnet's baseline behavior is controlled by the **ingress-isolation mode** of the namespace. Three values, set per namespace via the `kube-vnet/ingress-isolation` annotation, with cluster-wide defaults from operator flags:

| Mode | Baseline shape | Effect |
|---|---|---|
| `none` (default) | no baseline | The operator does not install a baseline. Ingress is unrestricted unless other policies (yours or the membership policies) restrict it. |
| `namespace` | ingress allowed from same-namespace pods | Pods in this namespace are reachable from other pods in the same namespace, but not from pods in other namespaces unless an explicit policy permits. |
| `pod` | strict ingress deny | No ingress allow rules in the baseline. Pods are reachable only via membership policies (vnet peers) or explicit user-managed policies. |

The baseline carries `policyTypes: [Ingress]` only. **Egress is unrestricted by the baseline.** Membership policies still grant egress allows to vnet peers; generic egress (DNS, the apiserver, the public internet, other namespaces) is not restricted by kube-vnet. If you need per-workload egress restriction, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` — see [`recipes.md`](recipes.md).

The baseline `NetworkPolicy` is named `kube-vnet-default-deny` and labeled `kube-vnet/managed-by=kube-vnet, kube-vnet/role=baseline`.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet-default-deny
  namespace: <ns>
  labels:
    kube-vnet/managed-by: kube-vnet
    kube-vnet/role: baseline
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress: <varies by mode>
```

See [ADR 0023](adr/0023-decoupled-disabled-and-ingress-isolation.md), [ADR 0024](adr/0024-ingress-isolation-mode-and-overrides.md), and [ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md).

### Resolving the mode for a namespace

Highest precedence wins:

1. The namespace's `kube-vnet/ingress-isolation` annotation, if set to a recognized value.
2. The operator-level override list the namespace appears in: `--ingress-isolation-none`, `--ingress-isolation-namespace`, or `--ingress-isolation-pod`.
3. Otherwise, the cluster-wide default `--ingress-isolation` (default `none`).

A namespace listed in two override lists is a startup configuration error; the operator refuses to start.

### Baseline ownership

The baseline lifecycle is owned by the **`NamespaceReconciler`**, which watches namespaces and applies or removes the baseline based on the resolved isolation mode. The `VirtualNetworkReconciler` only writes membership policies; it never touches the baseline. There is no implicit "first vnet member triggers the baseline" coupling — baseline existence is decided purely by the ingress-isolation mode resolved for the namespace.

### Disabling the operator for a namespace

The annotation `kube-vnet/disabled=true` is a separate, orthogonal switch. When set, the operator does nothing in that namespace: no baseline regardless of ingress-isolation, no membership policies, no eligibility as a peer, no honoring bindings. The operator-level flag `--disabled-namespaces` (default `[]`; the operator's own namespace is always added implicitly) has the same effect at the cluster level. The three control-plane namespaces (`kube-system`, `kube-public`, `kube-node-lease`) used to live here as defaults; they've moved to `operator.ingressIsolation.namespaceOverrides.none` so the operator can still discover deliberate joiners there but never installs an ingress baseline.

See [ADR 0006](adr/0006-baseline-default-deny-and-single-opt-out.md) (now superseded by ADR 0023 for the baseline-control half) and [ADR 0007](adr/0007-operator-level-excluded-namespaces.md).

---

## How the policies compose

NetworkPolicy in Kubernetes is **additive**. A pod's allowed traffic is the union of allow-rules from every policy that selects that pod. With kube-vnet's baseline + per-vnet membership policies in a namespace:

- **Pod with the join label, ingress-isolation `pod`**: both the baseline (deny-all-ingress) and the membership policy (allow same-vnet peers) select it. Net effect: ingress is restricted to same-vnet peers; egress is unrestricted.
- **Pod without the join label, ingress-isolation `pod`**: only the baseline selects it. Net effect: no ingress; egress is unrestricted.
- **Any pod, ingress-isolation `none`**: no baseline. Membership policies, if they select the pod, still restrict ingress to same-vnet peers (NetworkPolicy goes default-deny on a pod the moment any policy selects it for a given direction).

For cross-namespace ingress isolation, both ends need baselines (or the receiving side needs `ingress-isolation: pod`). A pod in A reaches a pod in B only if the receiving side allows it.

---

## The generated NetworkPolicy: per (vnet, namespace, direction class, label form)

For each VirtualNetwork with members, the operator generates **one membership `NetworkPolicy` per (namespace, direction class) with members**. In the home namespace, each direction class can additionally split into two policies — one matching the bare key, one matching the prefixed key — when both label forms are in use. Bindings produce one extra policy per binding.

Naming: `kube-vnet-<vnet>-<namespace>` for the bidirectional, bare-form, label-driven policy. Direction-suffixed (`-ingress` / `-egress`), form-suffixed (`-prefixed`), and binding-suffixed (`-b-<binding>`) variants apply otherwise. If the deterministic name exceeds Kubernetes' 253-character resource-name limit, the front is truncated and a 4-byte SHA-256 suffix is appended. See [ADR 0011](adr/0011-policy-naming-and-truncation.md).

Labels on every operator-managed `NetworkPolicy`:

- `kube-vnet/managed-by=kube-vnet` — claims operator ownership. Used by drift correction and cleanup.
- `kube-vnet/network=<homeNS>.<vnet>` — identifies which VirtualNetwork owns the policy. Used for cleanup, including cross-namespace.
- `kube-vnet/role=membership` (membership policies) or `=baseline` (baseline policies).
- `kube-vnet/binding=<binding>` — only on per-binding membership policies.

Owner references: only set when the policy is in the same namespace as the VirtualNetwork. Kubernetes does not support cross-namespace owner references. For policies in foreign namespaces, the operator manages cleanup via the `kube-vnet/network` label — see [ADR 0010](adr/0010-cross-namespace-cleanup-via-network-label.md).

This is why the operator can't do its job from a single cluster-scoped policy: stock `NetworkPolicy` is namespace-local. Each side needs its own policy. (For the future where this changes, see [ADR 0019](adr/0019-baseline-durability.md) on `AdminNetworkPolicy`.)

---

## Drift correction

The operator watches every `NetworkPolicy` carrying `kube-vnet/managed-by=kube-vnet`. If one is edited or deleted out-of-band:

- An update event fires, the policy's `kube-vnet/network` label maps it back to the owning VirtualNetwork, and the reconciler re-applies the desired spec via server-side apply with field manager `kube-vnet`.
- A delete event does the same: the reconciler re-creates the missing policy.
- On re-creation specifically (i.e. the policy was absent immediately before the apply), a `Warning PolicyRestored` Event is emitted on the owning VirtualNetwork so the deletion-and-restore cycle is visible in `kubectl describe vnet`.

Server-side apply is used with `client.ForceOwnership`, so the operator reliably reclaims field ownership on its own resources. See [ADR 0009](adr/0009-server-side-apply-with-field-manager.md) and [ADR 0019](adr/0019-baseline-durability.md).

**What drift correction does *not* do:** it can't prevent the deletion in the first place. There is a sub-second-to-a-few-seconds window where the policy is gone and traffic that the policy would have denied is allowed. Drift correction is a best-effort defense against accidental deletion, unaware tooling, and most non-malicious cases. For hard-guarantee namespace-RBAC-resistant deny rules, the proper Kubernetes tool is `AdminNetworkPolicy` — tracked in ADR 0019 as the future direction.

---

## Status conditions: what the operator tells you

Each VirtualNetwork carries two conditions in `status.conditions`:

- **`Ready`** — true when the desired NetworkPolicy set has been applied. False when something is preventing reconciliation (apply error, invalid name, home namespace excluded).
- **`Degraded`** — true when some subset of the desired state can't be honored (a labeled pod is in a non-permitted namespace, an unknown direction value, conflicting directions across the bare and prefixed label forms, or a name collision with a user-managed NetworkPolicy).

Each `VirtualNetworkBinding` similarly carries a `Ready` condition with reasons `PodsAttached`, `NoPodsMatch`, `VirtualNetworkNotFound`, `NamespaceNotAllowed`, `NamespaceExcluded`, `UnknownDirection`, or `InvalidSelector`.

Both conditions follow the standard `metav1.Condition` shape: `type`, `status`, `reason`, `message`, `lastTransitionTime`. Tools that consume this pattern (`kubectl wait --for=condition=Ready`, dashboards, event aggregators) work out of the box.

Transitions also emit Kubernetes Events. See [ADR 0012](adr/0012-status-conditions-ready-and-degraded.md) and [ADR 0016](adr/0016-emit-events-on-condition-transitions.md), and the full reason taxonomy in [`reference/api.md`](reference/api.md).

---

## Where to go from here

- **Try it**: [`install.md`](install.md) → [`recipes.md`](recipes.md).
- **Look up a field or value**: [`reference/`](reference/).
- **Run it in production**: [`operations.md`](operations.md), [`security.md`](security.md).
- **Debug it**: [`troubleshooting.md`](troubleshooting.md).
- **Read the design rationale**: [`adr/`](adr/README.md).
