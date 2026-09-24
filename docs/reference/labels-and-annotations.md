# Labels and annotations reference

Every label and annotation that kube-vnet writes, honors, or relies on. The keys are constants in `internal/controller/` (`policy_generator.go`, `namespace.go`, `resolution_controller.go`).

Convention ([ADR 0037](../adr/0037-system-prefix-convention-for-operator-owned-keys.md)): keys under `kube-vnet/` are **inputs you write**; keys under `kube-vnet.system/` are **written by the operator** and admission-protected — you can read and select on them, but not set, change, or remove them.

---

## Labels you put on pods

### `kube-vnet/net.<vnet-name>` (bare form)

| | |
|---|---|
| **On** | `Pod` (typically via the pod template) |
| **Value** | Exactly one of `both`, `ingress`, `egress`, `none` ([direction semantics](../getting-started/concepts.md#direction-modes-on-the-join-label)). The legacy `"true"`/`"false"`/empty aliases were dropped per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md). |
| **Meaning** | "This pod joins the VirtualNetwork `<vnet-name>` in the pod's own namespace, with the given direction." |
| **Accepted in** | The vnet's home namespace. For the system vnets (`namespace`, `cluster`) in any managed namespace. |

```yaml
labels:
  kube-vnet/net.payments: both     # pod in `platform`, vnet platform/payments
```

### `kube-vnet/net.<homeNS>.<vnet-name>` (prefixed form)

| | |
|---|---|
| **On** | `Pod` |
| **Value** | Same four directions as the bare form. |
| **Meaning** | "This pod joins the VirtualNetwork `<vnet-name>` in namespace `<homeNS>`, with the given direction." |
| **Accepted in** | Any namespace, including the home namespace. Required outside it. |
| **Honored only if** | The target vnet's `spec.allowedNamespaces` permits the pod's namespace. |

```yaml
labels:
  kube-vnet/net.platform.payments: both     # pod in `webapp`
```

VirtualNetwork names cannot contain dots (CRD CEL rule), so the two forms are unambiguous. Both forms canonicalize to the same key, so using both on one pod with different values intersects fail-closed ([ADR 0022](../adr/0022-long-form-join-label-in-home-namespace.md), [ADR 0033](../adr/0033-canonical-fq-system-labels.md)). Rationale: [ADR 0003](../adr/0003-one-label-per-virtualnetwork.md), [ADR 0004](../adr/0004-bare-vs-namespace-prefixed-join-label.md), [ADR 0021](../adr/0021-direction-modes-on-join-labels.md).

A join label is a *request*. Membership takes effect when the operator stamps the matching [`kube-vnet.system/net.*` label](#kube-vnetsystemnet-membership-stamp) on the pod.

### Validation and diagnostics

- **Admission** (Kubernetes ≥ 1.30): the chart's `<release>-join-label-direction` `ValidatingAdmissionPolicy` rejects a pod whose `kube-vnet/net.*` label value is not one of the four directions.
- **Pod events**: `InvalidDirection` for an unrecognized value (the label is ignored); `VirtualNetworkNotJoinable` when the named vnet doesn't exist or doesn't permit the pod's namespace — for a bare label naming a vnet that doesn't exist locally, the message hints at the prefixed form. Full list: [metrics-and-events § Kubernetes Events](metrics-and-events.md#kubernetes-events).
- **Vnet condition**: the vnet's `Degraded=True, reason=InvalidJoiners` names offending pods in namespaces it admits, with a per-pod reason (`InvalidDirection`, `NamespaceExcluded`).

Symptom-first walkthroughs: [troubleshooting § pod events](../guides/troubleshooting.md#pod-events-kube-vnet-emits).

---

## Annotations you put on pods

### `kube-vnet/network-max-wait`

| | |
|---|---|
| **On** | `Pod` (typically via the pod template) |
| **Value** | A positive Go duration, e.g. `"30s"`. |
| **Meaning** | "Hold this pod's app until every node has applied its NetworkPolicy rules, for at most this long." The webhook injects an init container, `kube-vnet-network-wait`, that runs first. When the maximum runs out, the app starts anyway. |
| **Requires** | `webhook.enabled` and `webhook.networkWait.enabled`, a managed namespace, and the annotation at pod creation. Otherwise, or with an invalid value, the pod starts without waiting and gets one `NetworkWaitSkipped` Warning saying why (and, where the webhook sees the pod, an admission warning that only its direct creator sees). hostNetwork pods are skipped silently. |

For one-shot clients, such as a Job whose first connection must succeed ([troubleshooting](../guides/troubleshooting.md#a-job-or-one-shot-pod-fails-to-connect-on-startup-but-succeeds-on-retry), [ADR 0045](../adr/0045-network-wait-for-opted-in-pods.md)).

```yaml
template:
  metadata:
    annotations:
      kube-vnet/network-max-wait: "30s"
```

---

## Labels and annotations the operator puts on pods

### `kube-vnet.system/net.*` (membership stamp)

| | |
|---|---|
| **On** | Every pod in a managed namespace with at least one resolved membership. |
| **Key** | `kube-vnet.system/net.<homeNS>.<vnet>`, except the cluster system vnet: `kube-vnet.system/net.cluster`. |
| **Value** | The resolved direction, always one of the bare four (`default-*` is consumed during resolution). |
| **Set by** | The `ResolutionReconciler`, or the mutating admission webhook when `webhook.enabled=true`. |
| **Meaning** | The confirmed membership after resolving all sources (cluster baseline → namespace baseline → bindings and join labels) and checking `allowedNamespaces`. |
| **Used by** | Every membership policy's `podSelector` and peer rules. This label, not the join label, is what the policies select on. |

Stripped when the namespace becomes disabled. See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md), [ADR 0031](../adr/0031-baseline-tier-resolution.md), [ADR 0033](../adr/0033-canonical-fq-system-labels.md).

### `kube-vnet.system/host-port.<port>.<protocol>=true`

| | |
|---|---|
| **On** | Pods declaring `hostPort` — one label per distinct `(port, protocol)` (protocol lowercase: `tcp`/`udp`/`sctp`). |
| **Set by** | Same writers as the membership stamp. |
| **Meaning** | Marks the pod as a hostPort exposer so the per-`(namespace, port, protocol)` policy `kube-vnet.ext.host.<port>.<proto>-<8hex>` can select it. Keying the policy on the port rather than the pod keeps it stable across rollouts. |
| **Used by** | The `HostPortReconciler`'s podSelector. `hostNetwork: true` pods are never stamped. See [ADR 0040](../adr/0040-auto-allow-hostport-pods.md). |

### `kube-vnet.system/resolved-generation` (annotation)

Written together with the stamp. Membership policies skip pods without it, so a pod that has not been resolved yet is never counted as a member (fail-closed). Removed when the namespace becomes disabled.

### `kube-vnet.system/resolved-by` (annotation)

Which path last stamped the pod: `admission` (the mutating webhook, [ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md)) or `controller` (the `ResolutionReconciler`). Diagnostic only; nothing branches on it. Seeing `controller` on a new pod while the webhook is enabled means the webhook was unreachable for that pod and the reconciler stamped it afterwards. Removed, like `resolved-generation`, when the namespace becomes disabled.

Annotations are not admission-protected; forging them grants nothing, because membership also requires the protected stamp.

---

## Annotations you put on namespaces and Services

### `kube-vnet/disabled`

| | |
|---|---|
| **On** | `Namespace` |
| **Value** | `"true"` opts out. Any other value (or absent) means managed. |
| **Meaning** | The operator does nothing in this namespace: no baseline, no `namespace` system vnet, no stamps, no membership or auto-allow policies; pods here can't join any vnet; bindings here are ignored. |
| **Used by** | `NamespaceFilter.IsManaged()`. The cluster-level equivalent is `--disabled-namespaces` / `operator.disabledNamespaces`. |

See [ADR 0006](../adr/0006-baseline-default-deny-and-single-opt-out.md) and [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md).

### `kube-vnet/external-allow`

| | |
|---|---|
| **On** | A `Service`, or a `Namespace` to cover everything in it. |
| **Value** | Only the literal `"false"` has an effect. |
| **Effect** | Opts out of the auto-allow families: `ext.svc` and `ext.apiserver` on a Service; all three (including `ext.host`) on a Namespace. Existing auto-allow policies are removed on the next reconcile. See [the auto-allow guide](../guides/auto-allow.md). |

### `kube-vnet/apiserver-reachable`

| | |
|---|---|
| **On** | A `Service`. |
| **Value** | Only the literal `"true"` has an effect. |
| **Effect** | Opts the Service **in** to the `ext.apiserver` auto-allow even when no webhook configuration, `APIService`, or CRD conversion webhook references it. See [ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md). |

### Removed: `kube-vnet/ingress-isolation`

A former namespace annotation selecting one of three baseline shapes. Removed by [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md); the baseline is now uniform. Tune the posture with the baseline tiers instead ([ADR 0031](../adr/0031-baseline-tier-resolution.md)).

---

## Labels the operator puts on its own resources

Don't put `kube-vnet.system/*` labels on your own objects — on clusters with the admission policies you can't, and without them the operator would treat the object as its own.

### `kube-vnet.system/managed-by=kube-vnet`

| | |
|---|---|
| **On** | Every operator-managed `NetworkPolicy` (baseline, membership, auto-allow) and the system `VirtualNetwork`s (`namespace`, `cluster`). |
| **Meaning** | **The authoritative ownership signal.** Sweeps, the uninstall cleanup hook, the metrics collector and the system-vnet trust checks key on it — safe only because the `kube-vnet.system/` prefix is admission-protected. |

### `app.kubernetes.io/managed-by=kube-vnet`

| | |
|---|---|
| **On** | Every operator-emitted object, alongside the label above. (Chart-rendered objects carry `app.kubernetes.io/managed-by: Helm` instead.) |
| **Meaning** | The [Kubernetes recommended label](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/), for dashboards and `kubectl get -l`. |
| **Used by** | Nothing in the operator. It is user-writable and cannot be admission-protected (Helm stamps it cluster-wide), so no sweep, cleanup, or trust decision keys on it. |

### `kube-vnet.system/network=<homeNS>.<vnet-name>`

| | |
|---|---|
| **On** | Every membership policy (`kube-vnet.mem.*`). Not on the baseline or auto-allow policies. |
| **Meaning** | "This NetworkPolicy belongs to `<homeNS>/<vnet-name>`." |
| **Used by** | `deleteMembershipPolicies`, which selects a vnet's policies cluster-wide — the substitute for cross-namespace owner references. See [ADR 0010](../adr/0010-cross-namespace-cleanup-via-network-label.md). |

### `kube-vnet.system/role`

| Value | On |
|---|---|
| `baseline` | `kube-vnet.base`. The `NamespaceReconciler` watches policies with this role to restore a deleted baseline. |
| `membership` | `kube-vnet.mem.*` — one per `(vnet, namespace)`, covering label-, binding- and baseline-driven members alike. |
| `external-allow` | `kube-vnet.ext.*` auto-allow policies. |

### `kube-vnet.system/source-kind` and `kube-vnet.system/source`

On auto-allow policies only. `source-kind` names the owning family (`svc` — [ADR 0038](../adr/0038-auto-allow-externally-exposed-services.md); `host` — [ADR 0040](../adr/0040-auto-allow-hostport-pods.md); `apiserver` — [ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md)); each reconciler sweeps only its own kind. `source` is the back-reference to the trigger: `svc-<service>`, `host-<port>-<proto>`, `apiserver-<service>`.

### Examples

```yaml
# Membership policy in webapp for vnet monitoring/observability
metadata:
  name: kube-vnet.mem.monitoring.observability-2b3c4d5e
  namespace: webapp
  labels:
    kube-vnet.system/managed-by: kube-vnet
    app.kubernetes.io/managed-by: kube-vnet
    kube-vnet.system/network: monitoring.observability
    kube-vnet.system/role: membership
---
# Baseline in webapp
metadata:
  name: kube-vnet.base
  namespace: webapp
  labels:
    kube-vnet.system/managed-by: kube-vnet
    app.kubernetes.io/managed-by: kube-vnet
    kube-vnet.system/role: baseline
---
# Auto-allow for cert-manager's webhook Service
metadata:
  name: kube-vnet.ext.apiserver.cert-manager-webhook-943e7fca
  namespace: cert-manager
  labels:
    kube-vnet.system/managed-by: kube-vnet
    app.kubernetes.io/managed-by: kube-vnet
    kube-vnet.system/role: external-allow
    kube-vnet.system/source-kind: apiserver
    kube-vnet.system/source: apiserver-cert-manager-webhook
```

---

## Generated selectors

A membership policy for vnet `<homeNS>/<vnet>` in a member namespace selects the receiver-capable members and admits the initiator-capable ones:

```yaml
spec:
  podSelector:
    matchExpressions:
      - { key: kube-vnet.system/net.<homeNS>.<vnet>, operator: In, values: [both, ingress] }
  policyTypes: [Ingress]
  ingress:
    - from:
        # one peer per member namespace that has initiators
        - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: <peerNS> } }
          podSelector:
            matchExpressions:
              - { key: kube-vnet.system/net.<homeNS>.<vnet>, operator: In, values: [both, egress] }
```

`egress`-only members get no policy of their own; they appear only as peers. A pod without the stamp, or stamped `none`, matches neither selector — this is what makes `allowedNamespaces` [join eligibility rather than blanket access](../getting-started/concepts.md#allowednamespaces-is-join-eligibility-not-blanket-access).

The peer rules rely on `kubernetes.io/metadata.name`, which Kubernetes sets on every namespace. `k8s-app=kube-dns` is used only by the chart's CoreDNS carve-out (`dnsCarveout.selector`), not by the operator.

---

## Labels on the operator's own Deployment

| Label | Value |
|---|---|
| `app.kubernetes.io/name` | `kube-vnet` |
| `app.kubernetes.io/component` | `controller` |
| `app.kubernetes.io/instance` | The Helm release name (chart only) |
| `app.kubernetes.io/managed-by` | `Helm` (chart) or absent (kustomize) |
| `app.kubernetes.io/version` | `Chart.appVersion` (chart only) |
| `helm.sh/chart` | `kube-vnet-<chart-version>` (chart only) |

The admission webhooks exclude pods labeled `app.kubernetes.io/name=kube-vnet`, so the operator never depends on itself to start.

---

## Quick lookup commands

```bash
# All operator-managed NetworkPolicies
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet

# Just the baselines
kubectl get networkpolicy -A -l kube-vnet.system/role=baseline

# The membership policies of one vnet
kubectl get networkpolicy -A -l kube-vnet.system/network=platform.payments

# A pod's confirmed memberships and who stamped them
kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep kube-vnet.system
kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.annotations.kube-vnet\.system/resolved-by}'

# Pods that stamped membership in platform/payments
kubectl get pods -A -l kube-vnet.system/net.platform.payments
```
