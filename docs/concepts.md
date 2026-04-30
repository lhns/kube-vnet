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

kube-vnet introduces that mental model on top of stock Kubernetes, without requiring CNI extensions. You declare a `VirtualNetwork`, services join it via labels, and the operator emits the underlying `NetworkPolicy` set.

For the longer "why a CRD at all" treatment, see [ADR 0001](adr/0001-virtualnetwork-as-named-network-abstraction.md).

---

## The mental model

A `VirtualNetwork` is a named group of pods that can communicate. Members of the group reach each other. Non-members (and members of other groups) cannot.

The "virtual" qualifier is deliberate: there is no separate network plane, no overlay, no tunnel. Traffic continues to flow through whatever CNI your cluster runs. The operator simply shapes the `NetworkPolicy` set so connectivity follows membership.

A pod can be a member of zero, one, or many VirtualNetworks. Membership composes additively at the pod level: a pod on networks A *and* B can reach any pod in A *or* B.

A `VirtualNetwork` is a Kubernetes resource. It lives in a "home" namespace. It can permit pods from other namespaces to join (see [`allowedNamespaces`](#cross-namespace-reach-allowednamespaces) below). It is **not** cluster-scoped; reach is a property of the network, not its identity. (See [ADR 0005](adr/0005-namespaced-crd-with-allowed-namespaces.md) for the rationale.)

---

## Joining: the label contract

Pods declare membership via **one label per joined VirtualNetwork**. The operator only inspects label *keys*; values are conventional `"true"` but the operator only checks for key presence.

Two label-key forms are recognized:

| Form | Used by | Example |
|---|---|---|
| **Bare** `kube-vnet/net.<vnet>` | Pods in the VirtualNetwork's home namespace | `kube-vnet/net.payments=true` |
| **Prefixed** `kube-vnet/net.<homeNS>.<vnet>` | Pods in any other namespace | `kube-vnet/net.platform.payments=true` |

The dot separator distinguishes the two forms. A single dot after `net.` means "in this pod's namespace"; two dots means "namespace-prefixed reference."

VirtualNetwork names cannot contain dots. The CRD enforces this at admission via a CEL rule (see [ADR 0017](adr/0017-name-validation-via-cel-and-runtime-check.md)). The encoding is therefore unambiguous.

Why one label per network rather than a comma-separated list? See [ADR 0003](adr/0003-one-label-per-virtualnetwork.md) — three concrete reasons (selector simplicity; 63-character label-value limit; matches the "one label per category" Kubernetes convention).

The full label form for cross-namespace references is documented in [ADR 0004](adr/0004-bare-vs-namespace-prefixed-join-label.md).

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

A pod in an `allowedNamespaces`-permitted namespace **must still add the join label** to become a member. Pods in those namespaces that don't carry the label get nothing.

The operator enforces this on both sides:

- **Discovery side** (`internal/controller/virtualnetwork_controller.go:discoverMembers`): the pod loop only considers a pod a member if it carries the appropriate join-label key. The `permits()` check on `allowedNamespaces` only runs for pods that already have the label.
- **NetworkPolicy side** (`internal/controller/policy_generator.go:Generate`): the generated `from` and `to` peer rules use `podSelector: { matchExpressions: [{ key: <join-key>, operator: Exists }] }`. Even at the policy layer, the only pods granted access are those carrying the join key.

For the full treatment, see [ADR 0005 § Join eligibility, not blanket access](adr/0005-namespaced-crd-with-allowed-namespaces.md).

### Why no glob patterns in `Names`

`Names` matches namespace names exactly. Globs like `payments-*` are deliberately unsupported because:

- Glob vs regex vs prefix is ambiguous and would have to be documented.
- Label selectors already cover "group of namespaces" cleanly.
- It's not idiomatic in Kubernetes APIs (cert-manager, Cilium, Istio do not accept globs in name lists).

If you want prefix-style matching, label your namespaces and use `selector`.

---

## The default-deny baseline

Without baseline isolation, `allowedNamespaces` and the membership rules above would be decorative. Kubernetes' default is allow-all: a pod with no `NetworkPolicy` selecting it can reach any other pod, regardless of vnet membership.

When at least one pod in a managed namespace joins a VirtualNetwork, the operator installs a `NetworkPolicy` named **`kube-vnet-default-deny`** in that namespace:

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
  podSelector: {}            # selects every pod in the namespace
  policyTypes: [Ingress, Egress]
  ingress: []                # no allow rules → deny all ingress
  egress:                    # only DNS to kube-system CoreDNS
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
```

The empty `podSelector` selects every pod in the namespace. The `policyTypes: [Ingress, Egress]` flips every selected pod from "default-allow for both directions" to "default-deny." The single egress rule ensures CoreDNS still works — without it, every kube-vnet-managed namespace would lose name resolution.

The baseline is **garbage-collected** when the last vnet member leaves the namespace. See [ADR 0006](adr/0006-baseline-default-deny-and-single-opt-out.md) for the rationale and [`docs/architecture.md`](architecture.md) for the lifecycle.

### Two ways to opt a namespace out

Both equivalent in effect:

- **Per-namespace annotation** `kube-vnet/disabled: "true"` on the `Namespace`. The operator does nothing in that namespace: no baseline, no membership policies, no eligibility as a peer.
- **Operator-level flag** `--excluded-namespaces=foo,bar`. Defaults: `kube-system,kube-public,kube-node-lease`. The operator's own namespace is always added implicitly. Same semantics as the per-namespace annotation.

Why both — see [ADR 0006](adr/0006-baseline-default-deny-and-single-opt-out.md) (per-namespace) and [ADR 0007](adr/0007-operator-level-excluded-namespaces.md) (operator-wide).

### `--default-deny-everywhere` (cluster-wide posture)

By default the operator is **opt-in per namespace**: a namespace gets the baseline only when at least one pod joins a vnet. Namespaces with no membership stay default-allow.

The flag `--default-deny-everywhere` flips this: when on, the baseline is installed in every non-excluded, non-disabled namespace, even those with no members. Use it when you want kube-vnet to be the cluster's network-policy story end-to-end.

See [ADR 0020](adr/0020-default-deny-unmanaged-namespaces.md) for the rationale and the migration pattern.

---

## How the policies compose

NetworkPolicy in Kubernetes is **additive**. A pod's allowed traffic is the union of allow-rules from every policy that selects that pod. With kube-vnet's baseline + per-vnet membership policies in a namespace:

- **Pod with the join label**: both the baseline (default-deny + DNS) and the membership policy (allow same-vnet peers) select it. Net effect: isolated except for same-vnet peers and DNS.
- **Pod without the join label**: only the baseline selects it. Net effect: isolated except DNS.

For cross-namespace isolation, both ends need baselines. A pod in A reaches a pod in B only if *both* sides' policies allow it — which they do, symmetrically, when both pods carry the right join labels for the same vnet.

If a namespace has *no* baseline (no pod there has joined any vnet *and* `--default-deny-everywhere` is off), it stays in default-allow mode. Its pods can still talk to anything that doesn't itself have a deny rule.

---

## The generated NetworkPolicy: one per (vnet, namespace-with-members)

For each VirtualNetwork with members, the operator generates **one `NetworkPolicy` per namespace that has members**.

Naming: `kube-vnet-<vnet>-<namespace>` (e.g. `kube-vnet-payments-platform`). If the deterministic name exceeds Kubernetes' 253-character resource-name limit, the front is truncated and a 4-byte SHA-256 suffix is appended. See [ADR 0011](adr/0011-policy-naming-and-truncation.md).

Labels on every operator-managed `NetworkPolicy`:

- `kube-vnet/managed-by=kube-vnet` — claims operator ownership. Used by drift correction and cleanup.
- `kube-vnet/network=<homeNS>.<vnet>` — identifies which VirtualNetwork owns the policy. Used for cleanup, including cross-namespace.
- `kube-vnet/role=membership` (membership policies) or `=baseline` (baseline policies).

Owner references: only set when the policy is in the same namespace as the VirtualNetwork. Kubernetes does not support cross-namespace owner references. For policies in foreign namespaces, the operator manages cleanup via the `kube-vnet/network` label — see [ADR 0010](adr/0010-cross-namespace-cleanup-via-network-label.md).

Spec shape, for a vnet `payments` in `platform` with one foreign member in `webapp`:

- **`kube-vnet-payments-platform`** (in platform): selects pods carrying `kube-vnet/net.payments`. Allows ingress from / egress to: pods in `platform` carrying `kube-vnet/net.payments`, and pods in `webapp` carrying `kube-vnet/net.platform.payments`. Plus DNS egress to kube-system.
- **`kube-vnet-payments-webapp`** (in webapp): selects pods carrying `kube-vnet/net.platform.payments`. Allows ingress from / egress to: pods in `platform` carrying `kube-vnet/net.payments`, and pods in `webapp` carrying `kube-vnet/net.platform.payments`. Plus DNS egress.

Two policies, two namespaces, symmetric. Each policy lives in and controls its own namespace. Cross-namespace coordination is done via the peer entries' `namespaceSelector + podSelector`. (The `namespaceSelector` uses the well-known `kubernetes.io/metadata.name` label, which Kubernetes 1.22+ guarantees on every Namespace.)

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
- **`Degraded`** — true when some subset of the desired state can't be honored (a labeled pod is in a non-permitted namespace, or in an excluded namespace, or there's a name collision with a user-managed NetworkPolicy).

Both conditions follow the standard `metav1.Condition` shape: `type`, `status`, `reason`, `message`, `lastTransitionTime`. Tools that consume this pattern (`kubectl wait --for=condition=Ready`, dashboards, event aggregators) work out of the box.

Transitions also emit Kubernetes Events on the VirtualNetwork. See [ADR 0012](adr/0012-status-conditions-ready-and-degraded.md) and [ADR 0016](adr/0016-emit-events-on-condition-transitions.md), and the full reason taxonomy in [`reference/api.md`](reference/api.md).

---

## Where to go from here

- **Try it**: [`install.md`](install.md) → [`recipes.md`](recipes.md).
- **Look up a field or value**: [`reference/`](reference/).
- **Run it in production**: [`operations.md`](operations.md), [`security.md`](security.md).
- **Debug it**: [`troubleshooting.md`](troubleshooting.md).
- **Read the design rationale**: [`adr/`](adr/README.md).
