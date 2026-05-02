# Labels and annotations reference

Every label and annotation that kube-vnet writes, honors, or relies on. Useful when you're auditing what kube-vnet *is* in your cluster, or building tooling that interacts with it.

---

## Labels you (the user) put on pods

These are how pods declare membership in `VirtualNetwork` resources.

### `kube-vnet/net.<vnet-name>` (bare form)

| | |
|---|---|
| **On** | `Pod` (typically via `Deployment.spec.template.metadata.labels`) |
| **Value** | One of `both` (default), `ingress`, `egress`, `none`. Legacy aliases: `"true"` → `both`, `"false"` → `none`. Unknown values surface on the vnet's `Degraded` condition with reason `UnknownDirection`. |
| **Set by** | The user / workload owner. |
| **Meaning** | "This pod is a member of the VirtualNetwork named `<vnet-name>` in the same namespace as this pod, with the given direction." |
| **Used by** | The operator's pod-watch + `discoverMembers` + the generated NetworkPolicy's `podSelector` and peer rules. |
| **Accepted in** | The vnet's home namespace only. Foreign-namespace pods must use the prefixed form. |

Example: a pod in `platform` joining the `payments` vnet (which lives in `platform`):

```yaml
labels:
  kube-vnet/net.payments: both
```

### `kube-vnet/net.<homeNS>.<vnet-name>` (prefixed form)

| | |
|---|---|
| **On** | `Pod` |
| **Value** | Same direction enum as the bare form: `both`, `ingress`, `egress`, `none` (or legacy `"true"`/`"false"`). |
| **Set by** | The user / workload owner. |
| **Meaning** | "This pod is a member of the VirtualNetwork named `<vnet-name>` in namespace `<homeNS>`, with the given direction." |
| **Used by** | Same as bare form, but works across namespaces. |
| **Accepted in** | Any namespace, including the vnet's home namespace. (Required for foreign namespaces; in the home namespace the bare form is also accepted.) |
| **Honored only if** | The target VirtualNetwork's `spec.allowedNamespaces` permits this pod's namespace. |

Example: a pod in `webapp` joining `payments` (which lives in `platform`):

```yaml
labels:
  kube-vnet/net.platform.payments: both
```

The dot separator distinguishes the two forms. Three or more dots in the part after `net.` would be ambiguous; VirtualNetwork names cannot contain dots (CRD CEL rule), so the encoding stays unambiguous.

**Long form in the home namespace.** A pod in the vnet's home namespace can use the prefixed form (or both forms simultaneously), which lets a templated workload reuse a single label key across namespaces. If a home-namespace pod carries both forms with conflicting direction values, the operator surfaces `Degraded`/`ConflictingDirections` and excludes the pod from membership. See [ADR 0022](../adr/0022-long-form-join-label-in-home-namespace.md).

For the rationale, see [ADR 0003](../adr/0003-one-label-per-virtualnetwork.md) (one label per network), [ADR 0004](../adr/0004-bare-vs-namespace-prefixed-join-label.md) (bare vs prefixed), and [ADR 0021](../adr/0021-direction-modes-on-join-labels.md) (direction values).

### Direction values: traffic-flow algebra

| Value | Meaning |
|---|---|
| `both` | Bidirectional. Accept ingress from peers; initiate egress to peers. |
| `ingress` | Accept-only. |
| `egress` | Initiate-only. |
| `none` | Not a member (equivalent to label absent). |

X→Y flows iff X is initiator-capable (`both`/`egress`) **and** Y is receiver-capable (`both`/`ingress`).

---

## Annotations you put on namespaces

### `kube-vnet/disabled`

| | |
|---|---|
| **On** | `Namespace` |
| **Value** | `"true"` to opt out. Any other value (including absent) means managed. |
| **Set by** | The cluster admin / namespace owner. |
| **Meaning** | "Operator: do nothing in this namespace." No baseline is created; no membership policies are generated for pods here; pods here are not eligible joiners for any VirtualNetwork (regardless of any vnet's `allowedNamespaces`). |
| **Used by** | `NamespaceFilter.IsManaged()` in the operator. The cluster-level mirror of this annotation is the `operator.disabledNamespaces` Helm value / `--disabled-namespaces` CLI flag. |

Example:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: legacy
  annotations:
    kube-vnet/disabled: "true"
```

See [ADR 0006](../adr/0006-baseline-default-deny-and-single-opt-out.md) (now superseded by ADR 0023 for the baseline-control half).

### `kube-vnet/ingress-isolation`

| | |
|---|---|
| **On** | `Namespace` |
| **Value** | `none` \| `namespace` \| `pod`. Any other value (including absent) means "fall back to operator-level config." |
| **Set by** | The cluster admin / namespace owner. |
| **Meaning** | The baseline mode for this namespace. `none` → no baseline. `namespace` → baseline allows ingress from same-namespace pods. `pod` → strict-ingress baseline (no allow rules). |
| **Used by** | `NamespaceReconciler.ResolveIsolation()` — the per-namespace annotation has the highest precedence; below it are the operator-level override lists, then the cluster-wide `--ingress-isolation` default. |
| **Independent of** | `kube-vnet/disabled` (the disabled annotation overrides everything regardless), and from vnet membership presence (no implicit "first member triggers baseline"). |

Example:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: payments
  annotations:
    kube-vnet/ingress-isolation: pod
```

See [ADR 0023](../adr/0023-decoupled-disabled-and-ingress-isolation.md), [ADR 0024](../adr/0024-ingress-isolation-mode-and-overrides.md), and [ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md).

---

## VirtualNetworkBinding: the no-label alternative

When a pod template can't be modified (third-party Helm chart, pod owned by another operator), use a `VirtualNetworkBinding` to enroll pods without writing to them. The binding selects pods *in its own namespace* via `spec.podSelector`.

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
  direction: both
  podSelector:
    matchLabels:
      app: thirdparty-billing-agent
```

The target vnet's `spec.allowedNamespaces` is still enforced; bindings in non-permitted (or `kube-vnet/disabled`) namespaces report `Ready=False` with the appropriate reason. See [ADR 0026](../adr/0026-virtualnetworkbinding-crd.md) and [`api.md`](api.md#virtualnetworkbinding).

---

## Labels the operator puts on its own resources

These are how the operator identifies what it owns. Don't put them on your own resources — the operator may treat that as drift on its own and overwrite/delete the resource.

### `kube-vnet/managed-by=kube-vnet`

| | |
|---|---|
| **On** | Every operator-managed `NetworkPolicy` (membership policies AND the baseline). |
| **Value** | Always `kube-vnet`. |
| **Set by** | The operator. |
| **Meaning** | "This NetworkPolicy is managed by kube-vnet. Drift correction applies." |
| **Used by** | The operator's NetworkPolicy watch predicate; `cleanupForDeleted`; `deleteStale`; `gcBaselineIfEmpty`; the `MetricsCollector`. Also referenced by the user-facing `kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet`. |

### `kube-vnet/network=<homeNS>.<vnet-name>`

| | |
|---|---|
| **On** | Every operator-generated **membership** policy (i.e. `kube-vnet-<vnet>-<ns>`). Does NOT appear on the baseline. |
| **Value** | `<homeNS>.<vnet-name>` — identifies the owning VirtualNetwork. The dot separator works because VirtualNetwork names can't contain dots. |
| **Set by** | The operator. |
| **Meaning** | "This NetworkPolicy belongs to `<homeNS>/<vnet-name>`." |
| **Used by** | `cleanupForDeleted` (selects all this vnet's policies cluster-wide) and `deleteStale`. The operator's solution to Kubernetes' lack of cross-namespace owner references. See [ADR 0010](../adr/0010-cross-namespace-cleanup-via-network-label.md). |

### `kube-vnet/binding=<binding-name>`

| | |
|---|---|
| **On** | Per-binding membership policies only — i.e. policies generated for a `VirtualNetworkBinding`. |
| **Value** | The binding's `metadata.name`. |
| **Set by** | The operator. |
| **Meaning** | "This NetworkPolicy is the binding-driven membership policy for the named binding." |
| **Used by** | Traceability and cleanup. `kubectl get networkpolicy -A -l kube-vnet/binding=<name>` returns exactly the policy a binding produced. |

Per-binding policies are named `kube-vnet-<vnet>-b-<binding>` and live in the binding's own namespace.

### `kube-vnet/role=membership` and `kube-vnet/role=baseline`

| | |
|---|---|
| **On** | `kube-vnet/role=membership` on per-vnet membership policies. `kube-vnet/role=baseline` on the `kube-vnet-default-deny` baseline. |
| **Set by** | The operator. |
| **Meaning** | Distinguishes the two policy classes. |
| **Used by** | `gcBaselineIfEmpty` (counts membership policies via this label to decide whether the baseline is still needed). |

Example: an operator-managed membership policy in `webapp` for vnet `monitoring/observability`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet-observability-webapp
  namespace: webapp
  labels:
    kube-vnet/managed-by: kube-vnet
    kube-vnet/network: monitoring.observability
    kube-vnet/role: membership
```

And the corresponding baseline in `webapp`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet-default-deny
  namespace: webapp
  labels:
    kube-vnet/managed-by: kube-vnet
    kube-vnet/role: baseline
```

---

## Standard Kubernetes labels the operator depends on

These are not kube-vnet-specific; the operator references them in generated peer rules.

### `kubernetes.io/metadata.name`

| | |
|---|---|
| **On** | Every `Namespace` |
| **Value** | The namespace's name. |
| **Set by** | Kubernetes (apiserver), automatic since k8s 1.22. |
| **Used by** | The generated NetworkPolicy peer rules use `namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: <peerNS> } }` to scope a peer to a specific namespace. |

If you're on a Kubernetes version older than 1.22 (you shouldn't be; we require ≥ 1.25), this label might not be present and the operator's policies won't match correctly. Modern cluster: not a concern.

### `k8s-app=kube-dns`

The operator no longer relies on this label. Earlier releases used it in the baseline's egress allow rule for CoreDNS; with the ingress-isolation rename ([ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md)) the baseline is `policyTypes: [Ingress]` only and egress is unrestricted, so DNS resolution works without an explicit allow.

---

## Selector keys the operator generates dynamically

Per `(vnet, namespace, direction class, label form)`, the operator generates a `NetworkPolicy` whose `podSelector` uses an `In` operator over the join label *value* to match the right direction class:

```yaml
podSelector:
  matchExpressions:
    - key: kube-vnet/net.<vnet>          # bare form (home namespace)
      # OR
      key: kube-vnet/net.<homeNS>.<vnet> # prefixed form
      operator: In
      values: [true, both]               # bidirectional policy
      # OR
      values: [ingress]                  # ingress-only policy
      # OR
      values: [egress]                   # egress-only policy
```

Peer rules apply the same value-narrowing on the *other* side: an `ingress` allow on Y selects peers that can initiate egress (`In [true, both, egress]`); an `egress` allow on Y selects peers that can accept ingress (`In [true, both, ingress]`).

This is what enforces "join eligibility, not blanket access" at the policy level: a pod in the namespace without the join label, or with `value=none`, doesn't match the selector and gets nothing from the membership policy.

---

## Labels the operator's *own* Deployment uses

Standard `app.kubernetes.io/*` labels on the operator Deployment, ServiceAccount, etc:

| Label | Value |
|---|---|
| `app.kubernetes.io/name` | `kube-vnet` |
| `app.kubernetes.io/component` | `controller` |
| `app.kubernetes.io/instance` | The Helm release name (chart only) |
| `app.kubernetes.io/managed-by` | `Helm` (chart) or absent (kustomize) |
| `app.kubernetes.io/version` | `Chart.appVersion` (chart only) |
| `helm.sh/chart` | `kube-vnet-<chart-version>` (chart only) |

These follow Kubernetes' [Recommended Labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/) — used by tools like Lens, k9s, and `kubectl get -l`.

---

## Quick lookup commands

```bash
# All operator-managed NetworkPolicies cluster-wide
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet

# Just the baselines
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet,kube-vnet/role=baseline

# Just the membership policies for a specific vnet
kubectl get networkpolicy -A -l kube-vnet/network=platform.payments

# Pods in webapp that are members of any vnet
kubectl get pods -n webapp -L kube-vnet/net.payments,kube-vnet/net.monitoring,...

# Find pods with any kube-vnet join label (across all namespaces)
kubectl get pods -A --show-labels \
  | awk 'NR==1 || $6 ~ /kube-vnet\/net\./'
```
