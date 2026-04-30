# Labels and annotations reference

Every label and annotation that kube-vnet writes, honors, or relies on. Useful when you're auditing what kube-vnet *is* in your cluster, or building tooling that interacts with it.

---

## Labels you (the user) put on pods

These are how pods declare membership in `VirtualNetwork` resources.

### `kube-vnet/net.<vnet-name>` (bare form)

| | |
|---|---|
| **On** | `Pod` (typically via `Deployment.spec.template.metadata.labels`) |
| **Value** | Conventional `"true"`. The operator checks key presence only — any non-empty value works. |
| **Set by** | The user / workload owner. |
| **Meaning** | "This pod is a member of the VirtualNetwork named `<vnet-name>` in the same namespace as this pod." |
| **Used by** | The operator's pod-watch + `discoverMembers` + the generated NetworkPolicy's `podSelector` and peer rules. |

Example: a pod in `platform` joining the `payments` vnet (which lives in `platform`):

```yaml
labels:
  kube-vnet/net.payments: "true"
```

### `kube-vnet/net.<homeNS>.<vnet-name>` (prefixed form)

| | |
|---|---|
| **On** | `Pod` |
| **Value** | Conventional `"true"`. Key-presence check only. |
| **Set by** | The user / workload owner. |
| **Meaning** | "This pod (in some other namespace) is a member of the VirtualNetwork named `<vnet-name>` in namespace `<homeNS>`." |
| **Used by** | Same as bare form, but for cross-namespace membership. |
| **Honored only if** | The target VirtualNetwork's `spec.allowedNamespaces` permits this pod's namespace. |

Example: a pod in `webapp` joining `payments` (which lives in `platform`):

```yaml
labels:
  kube-vnet/net.platform.payments: "true"
```

The dot separator distinguishes the two forms. Three or more dots in the part after `net.` would be ambiguous; VirtualNetwork names cannot contain dots (CRD CEL rule), so the encoding stays unambiguous.

For the rationale, see [ADR 0003](../adr/0003-one-label-per-virtualnetwork.md) (one label per network) and [ADR 0004](../adr/0004-bare-vs-namespace-prefixed-join-label.md) (bare vs prefixed).

---

## Annotations you put on namespaces

### `kube-vnet/disabled`

| | |
|---|---|
| **On** | `Namespace` |
| **Value** | `"true"` to opt out. Any other value (including absent) means managed. |
| **Set by** | The cluster admin / namespace owner. |
| **Meaning** | "Operator: do nothing in this namespace." No baseline is created; no membership policies are generated for pods here; pods here are not eligible joiners for any VirtualNetwork (regardless of any vnet's `allowedNamespaces`). |
| **Used by** | `NamespaceFilter.IsManaged()` in the operator. |

Example:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: legacy
  annotations:
    kube-vnet/disabled: "true"
```

See [ADR 0006](../adr/0006-baseline-default-deny-and-single-opt-out.md).

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

| | |
|---|---|
| **On** | CoreDNS pods in `kube-system`. Standard for nearly every Kubernetes distribution. |
| **Used by** | The baseline's egress allowance: `egress.to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } }, podSelector: { matchLabels: { k8s-app: kube-dns } } }]`. |

Without this label on CoreDNS, the baseline doesn't allow DNS to it and pods in vnet-managed namespaces lose name resolution. In practice every distribution sets it; if your cluster doesn't, set it manually or kube-vnet won't work.

---

## Selector keys the operator generates dynamically

Per `(vnet, namespace)`, the operator generates a `NetworkPolicy` whose `podSelector` and peer rules use an `Exists` operator on the appropriate join label key:

```yaml
podSelector:
  matchExpressions:
    - key: kube-vnet/net.<vnet>          # if this is the home namespace
      # OR
      key: kube-vnet/net.<homeNS>.<vnet> # if this is a foreign namespace
      operator: Exists
```

Same pattern in each peer's `podSelector`. This is what enforces "join eligibility, not blanket access" at the policy level: a pod in the namespace without the join label doesn't match the selector and gets nothing from the membership policy.

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
