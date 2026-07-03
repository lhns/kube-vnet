# Labels and annotations reference

Every label and annotation that kube-vnet writes, honors, or relies on. Useful when you're auditing what kube-vnet *is* in your cluster, or building tooling that interacts with it.

---

## Labels you (the user) put on pods

These are how pods declare membership in `VirtualNetwork` resources.

### `kube-vnet/net.<vnet-name>` (bare form)

| | |
|---|---|
| **On** | `Pod` (typically via `Deployment.spec.template.metadata.labels`) |
| **Value** | Exactly one of `both` (default), `ingress`, `egress`, `none`. Legacy aliases (`"true"`, `"false"`, empty string) were dropped per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md). Unknown values are rejected at admission on Kubernetes ≥ 1.30 (the chart's `ValidatingAdmissionPolicy`); on older clusters they are admitted but surface on the vnet's `Degraded` condition with reason `UnknownDirection`. |
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
| **Value** | Same direction enum as the bare form: `both`, `ingress`, `egress`, `none`. Legacy aliases dropped per ADR 0030. |
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

**Long form in the home namespace.** A pod in the vnet's home namespace can use the prefixed form (or both forms simultaneously), which lets a templated workload reuse a single label key across namespaces. Both inputs canonicalize to the same FQ key at stamp time per [ADR 0033](../adr/0033-canonical-fq-system-labels.md), so disagreeing direction values on the two forms intersect fail-closed (no separate `ConflictingDirections` reason). See [ADR 0022](../adr/0022-long-form-join-label-in-home-namespace.md).

For the rationale, see [ADR 0003](../adr/0003-one-label-per-virtualnetwork.md) (one label per network), [ADR 0004](../adr/0004-bare-vs-namespace-prefixed-join-label.md) (bare vs prefixed), and [ADR 0021](../adr/0021-direction-modes-on-join-labels.md) (direction values).

### Direction values: traffic-flow algebra

| Value | Meaning |
|---|---|
| `both` | Bidirectional. Accept ingress from peers; initiate egress to peers. |
| `ingress` | Accept-only. |
| `egress` | Initiate-only. |
| `none` | Not a member (equivalent to label absent). |

X→Y flows iff X is initiator-capable (`both`/`egress`) **and** Y is receiver-capable (`both`/`ingress`).

### Diagnostics

When a join label is present but membership can't be honored, kube-vnet surfaces the cause on the *Pod* (Warning event) so the pod owner sees it via `kubectl describe pod`. Three reasons:

| Reason | Meaning |
|---|---|
| `BareJoinLabelVnetNotFound` | Pod has the bare form `kube-vnet/net.<X>` but no `VirtualNetwork` named `<X>` exists in the pod's own namespace (or the pod is in a foreign namespace where the bare form is not recognized). |
| `PrefixedJoinLabelVnetNotFound` | Pod has `kube-vnet/net.<homeNS>.<X>` but the vnet `<homeNS>/<X>` doesn't exist. |
| `JoinLabelNamespaceNotAllowed` | The vnet exists at the named home, but its `spec.allowedNamespaces` does not permit the pod's namespace. |

In addition, on Kubernetes ≥ 1.30 the chart installs a `ValidatingAdmissionPolicy` that rejects Pod create/update when any `kube-vnet/net.*` label has a value not in `[both, ingress, egress, none]`. On older clusters the same condition still surfaces at reconcile time as `Degraded`/`UnknownDirection` on the vnet.

See [ADR 0027](../adr/0027-pod-scoped-join-label-events.md) and [`../troubleshooting.md`](../guides/troubleshooting.md#pod-events-kube-vnet-emits).

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

### ~~`kube-vnet/ingress-isolation`~~ — *removed*

Previously a namespace annotation that selected one of three baseline shapes (`none`/`namespace`/`pod`). Removed under [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md): the baseline is now uniformly deny-all selecting every pod, with no per-namespace shape knob and no elide-list (the `--elide-baseline-for` flag from ADR 0030 was removed in [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md) — it had no observable effect). Tune behaviour via `ClusterVirtualNetworkBaseline` (cluster scope; chart-seeded from `operator.clusterBaseline.ingressIsolationLevel`), `VirtualNetworkBaseline` (per-NS), `VirtualNetworkBinding` (per-workload), or `kube-vnet/net.<vnet>` labels (per-pod). See [ADR 0031](../adr/0031-baseline-tier-resolution.md). To opt a namespace out entirely, use `kube-vnet/disabled=true` above.

### `kube-vnet/net.<vnet>=<direction>` (pod label) — direction values

Pod-tier sources (the `kube-vnet/net.<vnet>` pod label and `VirtualNetworkBinding.spec.direction`) accept four bare values:

- `both` — pod is both an ingress receiver and an egress sender for that vnet.
- `ingress` — pod accepts ingress from vnet members.
- `egress` — pod can send egress to vnet members.
- `none` — pod is explicitly NOT on this vnet (overrides any inherited default).

Baseline tiers (`ClusterVirtualNetworkBaseline`, `VirtualNetworkBaseline`) accept the same four bare values **plus** four `default-*` variants (`default-both`, `default-ingress`, `default-egress`, `default-none`). The `default-*` prefix marks the value as **override-permitted by lower tiers**. Bare values at a baseline are **enforced**; lower tiers attempting to override are rejected and the upstream value remains effective. The final stamped pod label is always one of the bare four — the `default-*` prefix is consumed during resolution. See [ADR 0031](../adr/0031-baseline-tier-resolution.md).

### Vnet-key forms in baselines

Pod labels admit two forms for the vnet suffix in `kube-vnet/net.<key>`: bare (`<name>`, "vnet of this name in this pod's namespace") and prefixed (`<homeNS>.<name>`, an explicit cross-namespace reference). Baselines use the same syntax in their `memberships` (the chart's `operator.clusterBaseline.memberships` map shorthand mirrors it), but the semantics are subtly different at the baseline tier:

- A **bare key** in a baseline names a *scope-relative* vnet — only meaningful for the system vnets `namespace` and `cluster`. The reserved-name VAP guarantees no user vnet can collide. Bare-keyed entries expand to the matching bare pod label at resolution time, so a single baseline entry produces a per-pod-namespace effect for the per-NS `namespace` system vnet.
- A **prefixed key** (`<namespace>.<name>`) is fully resolved at the baseline level. Use this for user vnets you want to attach via a baseline rather than per-workload bindings.
- Specifying the `cluster` system vnet with a `<release-namespace>.cluster` (or any `<X>.cluster`) prefixed form is accepted but normalized to bare `cluster` per [ADR 0033 Amendment](../adr/0033-canonical-fq-system-labels.md): the cluster vnet is a singleton, the prefix is informationless, and the canonicalization rule collapses `<anything>.cluster` to bare. Pod-stamped labels and policy names always use the bare form.

### Validation limits on baseline references

CRD CEL on baseline kinds cannot validate that the referenced vnets exist — admission-time CEL only sees the document being admitted, and a webhook that read other resources would race with vnet creation/deletion. A reference to a non-existent vnet is accepted at admission and silently becomes a no-op at pod-resolution time (no `kube-vnet.system/net.<vnet>` label is stamped for that membership). Bare-keyed entries are even more dynamic — the effective vnet differs per pod's namespace, so a single baseline entry can be valid for some pods and a no-op for others. Validation surfaces at pod-resolution: baseline status conditions (`Conflicts`, `OverrideRejected`) and missing system labels on pods that should have inherited them. Use `kubectl get vnet -A` to confirm references resolve.

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

### `kube-vnet.system/managed-by=kube-vnet`

| | |
|---|---|
| **On** | Every operator-managed `NetworkPolicy` (membership policies AND the baseline). |
| **Value** | Always `kube-vnet`. |
| **Set by** | The operator. |
| **Meaning** | "This NetworkPolicy is managed by kube-vnet. Drift correction applies." **The authoritative ownership signal** — sweeps, the uninstall cleanup hook, and the system-vnet trust checks all key on this label, which is safe only because the `kube-vnet.system/*` prefix is admission-protected (users can't set or strip it). |
| **Used by** | The operator's NetworkPolicy watch predicates (in both reconcilers); `cleanupForDeleted`; `deleteStale`; the `MetricsCollector`. Also referenced by the user-facing `kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet`. |

### `app.kubernetes.io/managed-by=kube-vnet`

| | |
|---|---|
| **On** | Every operator-emitted resource (all NetworkPolicy families and the system VirtualNetworks), alongside the system label above. |
| **Value** | `kube-vnet`. |
| **Set by** | The operator. (Chart-templated resources — the Deployment, RBAC, VAPs, CRDs — carry `app.kubernetes.io/managed-by: Helm` instead, because Helm manages those.) |
| **Meaning** | The [Kubernetes recommended label](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/), stamped purely for ecosystem convention: dashboards and `kubectl get -l app.kubernetes.io/managed-by=kube-vnet` work as users expect. |
| **Used by** | Nothing in the operator. **Informational only** — this key is user-writable by design and cannot be admission-protected (Helm stamps it cluster-wide), so no sweep, cleanup, or trust decision ever keys on it; a policy carrying only this label is never touched by kube-vnet. The system label above is the sole authoritative signal. |

### `kube-vnet.system/network=<homeNS>.<vnet-name>`

| | |
|---|---|
| **On** | Every operator-generated **membership** policy (i.e. `kube-vnet-<vnet>-<ns>`). Does NOT appear on the baseline. |
| **Value** | `<homeNS>.<vnet-name>` — identifies the owning VirtualNetwork. The dot separator works because VirtualNetwork names can't contain dots. |
| **Set by** | The operator. |
| **Meaning** | "This NetworkPolicy belongs to `<homeNS>/<vnet-name>`." |
| **Used by** | `cleanupForDeleted` (selects all this vnet's policies cluster-wide) and `deleteStale`. The operator's solution to Kubernetes' lack of cross-namespace owner references. See [ADR 0010](../adr/0010-cross-namespace-cleanup-via-network-label.md). |

### `kube-vnet.system/role=membership` and `kube-vnet.system/role=baseline`

| | |
|---|---|
| **On** | `kube-vnet.system/role=membership` on the per-`(vnet, namespace)` membership policy (covers label-driven and binding-driven members alike — there is no separate per-binding policy per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)). `kube-vnet.system/role=baseline` on the `kube-vnet` baseline. |
| **Set by** | The operator. |
| **Meaning** | Discriminates the two policy classes the operator owns. |
| **Used by** | (1) The `NamespaceReconciler` watches `NetworkPolicy` events with `role=baseline` so a manual delete of `kube-vnet` is re-applied within one reconcile cycle. (2) Tests scope assertions by it (e.g. `TestE2E_VNetDelete_BlocksTraffic` polls for `role=membership` cleanup separately from baseline lifecycle). (3) `kubectl get netpol -A -l kube-vnet.system/role=baseline` is the standard way to enumerate baseline policies cluster-wide. |

Example: an operator-managed membership policy in `webapp` for vnet `monitoring/observability`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.mem.monitoring.observability-2b3c4d5e
  namespace: webapp
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/network: monitoring.observability
    kube-vnet.system/role: membership
```

And the corresponding baseline in `webapp`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.base
  namespace: webapp
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/role: baseline
```

### `kube-vnet.system/role=external-allow` + `source-kind` + `source`

| | |
|---|---|
| **On** | Every auto-allow policy (`kube-vnet.ext.*`) emitted by the [auto-allow reconcilers](../guides/auto-allow.md). |
| **Set by** | The operator. |
| **Meaning** | `role=external-allow` marks the policy class; `source-kind` names which reconciler family owns it (`svc` — externally-exposed Service, ADR 0038; `host` — hostPort pod, ADR 0040; `apiserver` — Service the apiserver dials, ADR 0041); `source` is the back-reference to the triggering identity (`svc-<serviceName>`, `host-<port>-<proto>`, `apiserver-<serviceName>`). |
| **Used by** | Each reconciler filters its cleanup sweep by `source-kind` so it never touches another family's policies; drift watches re-enqueue the source object from these labels; `kubectl get netpol -A -l kube-vnet.system/role=external-allow` enumerates every auto-allow in the cluster. |

Example — the policy emitted for cert-manager's webhook Service:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.ext.apiserver.cert-manager-webhook-943e7fca
  namespace: cert-manager
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/role: external-allow
    kube-vnet.system/source-kind: apiserver
    kube-vnet.system/source: apiserver-cert-manager-webhook
```

### `kube-vnet.system/host-port.<port>.<protocol>=true`

| | |
|---|---|
| **On** | Pods declaring `hostPort` — one label per distinct `(port, protocol)` the pod exposes (protocol lowercase: `tcp`/`udp`/`sctp`). |
| **Set by** | The resolution controller (stamped, like the `net.*` membership labels; VAP-protected). |
| **Meaning** | Marks the pod as a hostPort exposer so the per-`(namespace, port, protocol)` policy `kube-vnet.ext.host.<port>.<proto>-<8hex>` can select it. Keying the policy on the port rather than the pod keeps policies stable across Deployment rollouts. |
| **Used by** | The `HostPortReconciler`'s podSelector. `hostNetwork: true` pods are never stamped (NetworkPolicy enforcement on them is CNI-dependent). |

---

## Annotations on Services

### `kube-vnet/external-allow`

| | |
|---|---|
| **On** | A `Service` — or a `Namespace` to cover every Service in it. |
| **Set by** | You. |
| **Value** | Only the literal `"false"` has an effect. Absent / empty / `"true"` / anything else leaves auto-allow on (deliberately asymmetric: this is an explicit opt-out signal, not a boolean). |
| **Effect** | Opts the Service (or namespace) out of **all** auto-allow families — the `ext.svc.*` policy for external exposure AND the `ext.apiserver.*` policy for apiserver-dialed backends. Existing auto-allow policies are removed on the next reconcile; you take over reachability with your own NetworkPolicy. See [the auto-allow guide](../guides/auto-allow.md). |

### `kube-vnet/apiserver-reachable`

| | |
|---|---|
| **On** | A `Service`. |
| **Set by** | You. |
| **Value** | Only the literal `"true"` has an effect. |
| **Effect** | Opts the Service **in** to the apiserver-reachable auto-allow (`kube-vnet.ext.apiserver.*`) even when none of the four built-in discovery resources (Validating/MutatingWebhookConfiguration, APIService, CRD conversion) reference it. The escape hatch for future Kubernetes APIs or third-party operators whose callback registration kube-vnet doesn't know about. See [ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md). |

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

Per `(vnet, namespace, key-form)`, the operator generates **one** ingress-only `NetworkPolicy` whose `podSelector` selects every receiver-capable member via an `In` operator over the join label *value*:

```yaml
podSelector:
  matchExpressions:
    - key: kube-vnet/net.<vnet>          # bare form (home namespace)
      # OR
      key: kube-vnet/net.<homeNS>.<vnet> # prefixed form
      operator: In
      values: [both, ingress]           # all receiver-capable members
```

`egress`-only members are deliberately not in this set — they accept no ingress, and the operator no longer restricts egress (ADR 0025), so there's nothing to allow on a self-policy. They still appear as *peer initiators* in other members' ingress.from rules.

Peer rules narrow to initiator-capable members on the source side: `ingress.from` selects peers via `kube-vnet.system/net.<homeNS>.<vnet> In [both, egress]`. (The bidi+ingress merge is documented in [ADR 0021 Addendum](../adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-04--bidi--ingress-self-policies-merged); the older split into separate `-ingress` / `-egress` self-policies is gone.)

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
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet

# Just the baselines
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet,kube-vnet.system/role=baseline

# Just the membership policies for a specific vnet
kubectl get networkpolicy -A -l kube-vnet.system/network=platform.payments

# Pods in webapp that are members of any vnet
kubectl get pods -n webapp -L kube-vnet/net.payments,kube-vnet/net.monitoring,...

# Find pods with any kube-vnet join label (across all namespaces)
kubectl get pods -A --show-labels \
  | awk 'NR==1 || $6 ~ /kube-vnet\/net\./'
```
