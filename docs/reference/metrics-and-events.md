# Metrics and Events reference

Every Prometheus metric the operator exposes, every Kubernetes Event reason it emits, and sample queries for each.

For where to scrape and how to alert, see [`../operations.md`](../operations.md#observability).

---

## Metrics

The operator exposes Prometheus text format on `:8080/metrics`. Six domain-specific metrics on top of the controller-runtime defaults (`workqueue_*`, `controller_runtime_*`, `rest_client_*`, Go runtime — those are documented [upstream](https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/metrics/leaderelection.go)).

### `kube_vnet_reconciliations_total`

| | |
|---|---|
| **Type** | Counter |
| **Labels** | `result` ∈ `success` \| `error` |
| **Description** | Total `VirtualNetwork` reconciliations by outcome. |
| **When it changes** | Once per `Reconcile` call, in `defer observeReconcile(...)`. |

Sample query — reconcile error rate:

```promql
sum(rate(kube_vnet_reconciliations_total{result="error"}[5m]))
  / sum(rate(kube_vnet_reconciliations_total[5m]))
```

### `kube_vnet_reconcile_duration_seconds`

| | |
|---|---|
| **Type** | Histogram |
| **Labels** | none |
| **Description** | Wall-clock duration of `VirtualNetwork` reconcile calls in seconds. Default Prometheus buckets. |
| **When it changes** | Once per `Reconcile` call, in `defer observeReconcile(...)`. |

Sample query — p95 reconcile latency:

```promql
histogram_quantile(0.95, rate(kube_vnet_reconcile_duration_seconds_bucket[5m]))
```

### `kube_vnet_networks_total`

| | |
|---|---|
| **Type** | Gauge |
| **Labels** | none |
| **Description** | Number of `VirtualNetwork` resources observed in the cluster. |
| **When it changes** | Updated by `MetricsCollector` every 30 seconds (lists `VirtualNetwork` cluster-wide). |

Sample query — current vnet count:

```promql
kube_vnet_networks_total
```

### `kube_vnet_managed_policies_total`

| | |
|---|---|
| **Type** | Gauge |
| **Labels** | none |
| **Description** | Number of `NetworkPolicy` resources currently labeled `kube-vnet/managed-by=kube-vnet` (membership policies + baselines combined). |
| **When it changes** | Updated by `MetricsCollector` every 30 seconds. |

Sample query — current managed-policy count:

```promql
kube_vnet_managed_policies_total
```

### `kube_vnet_members_total`

| | |
|---|---|
| **Type** | Gauge |
| **Labels** | `network` — `<homeNamespace>/<vnetName>` |
| **Description** | Total pod members per VirtualNetwork. |
| **When it changes** | At the end of each successful `Reconcile`, set to the sum of `len(MembersByNS[ns])` across all member-bearing namespaces. Cleared on vnet deletion via `clearMembers`. |

Sample query — top 5 vnets by member count:

```promql
topk(5, kube_vnet_members_total)
```

### `kube_vnet_apply_errors_total`

| | |
|---|---|
| **Type** | Counter |
| **Labels** | `kind` ∈ `membership_policy` \| `baseline` |
| **Description** | Total apply errors by policy kind. Increments when an SSA `Patch` returns an error. |
| **When it changes** | At the failure site of each apply call. |

Sample query — recent apply errors:

```promql
increase(kube_vnet_apply_errors_total[5m]) > 0
```

---

## Sample alert rules

A starter set. Tune thresholds for your environment.

```yaml
groups:
  - name: kube-vnet
    rules:
      # Any apply error in the last 5 minutes is worth a look.
      - alert: KubeVnetApplyErrors
        expr: increase(kube_vnet_apply_errors_total[5m]) > 0
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "kube-vnet failed to apply NetworkPolicies"
          description: "{{ $value }} apply errors in the last 5m."

      # Sustained reconcile failure rate above 10%.
      - alert: KubeVnetReconcileErrorRate
        expr: |
          sum(rate(kube_vnet_reconciliations_total{result="error"}[5m]))
            / sum(rate(kube_vnet_reconciliations_total[5m])) > 0.1
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: "kube-vnet reconcile error rate above 10%"

      # Slow reconciles — usually high vnet count or apiserver pressure.
      - alert: KubeVnetSlowReconcile
        expr: |
          histogram_quantile(0.95,
            rate(kube_vnet_reconcile_duration_seconds_bucket[5m])) > 5
        for: 15m
        labels: { severity: info }
        annotations:
          summary: "kube-vnet p95 reconcile latency > 5s"

      # Repeated PolicyRestored events suggest someone is fighting the operator.
      - alert: KubeVnetPolicyRestoredRepeatedly
        # Requires kube-state-metrics. Counts events labeled reason=PolicyRestored.
        expr: |
          sum by (namespace) (
            increase(kube_events{reason="PolicyRestored"}[15m])
          ) > 5
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: "kube-vnet keeps restoring deleted NetworkPolicies in {{ $labels.namespace }}"
```

---

## Kubernetes Events

The operator emits Events on every VirtualNetwork it reconciles. Events have a default TTL of 1 hour (apiserver-managed); they're a notification mechanism, not a durable audit log. The VirtualNetwork's status conditions are the source of truth for current state.

### VirtualNetwork event reasons

| Reason | Type | When it fires |
|---|---|---|
| `Ready` | Normal | `Ready` condition transitions False → True. The current condition message is the event message. |
| `NotReady` | Warning | `Ready` condition transitions True → False. |
| `Degraded` | Warning | `Degraded` condition transitions False → True. |
| `Recovered` | Normal | `Degraded` condition transitions True → False. |
| `ApplyFailed` | Warning | A `NetworkPolicy` apply call returned an error. Fires immediately at the failure site, regardless of subsequent condition state. The event message includes the policy ref and the apiserver error. |
| `PolicyRestored` | Warning | The operator just re-created a `NetworkPolicy` that was absent immediately before its apply call. Indicates an out-of-band deletion was detected and reverted. The message includes the policy ref. See [ADR 0019](../adr/0019-baseline-durability.md) and [`../security.md`](../security.md). |

The condition reasons that drive these events include `PoliciesGenerated`, `NoMembers`, `InvalidJoiners`, `UnknownDirection`, `ConflictingDirections`, `InvalidName`, `HomeNamespaceExcluded`, `NamespaceNotAllowed`, `NamespaceExcluded`, `ApplyFailed`, `NoIssues`. The Go-level constants are in `internal/controller/virtualnetwork_controller.go` (`Reason*`).

(A `NameCollision` event reason is planned alongside the same-named `Degraded` reason for the case where a user-managed `NetworkPolicy` blocks an operator name.)

### Pod event reasons (join-label diagnostics)

Emitted by the `JoinLabelDiagnosticReconciler` on the Pod object in the pod's own namespace. All Warning. See [ADR 0027](../adr/0027-pod-scoped-join-label-events.md).

| Reason | Type | Scope | When it fires |
|---|---|---|---|
| `BareJoinLabelVnetNotFound` | Warning | Pod | Pod carries `kube-vnet/net.<X>` but no `VirtualNetwork` of name `<X>` exists in the pod's own namespace. |
| `PrefixedJoinLabelVnetNotFound` | Warning | Pod | Pod carries `kube-vnet/net.<homeNS>.<X>` but the vnet `<homeNS>/<X>` does not exist. |
| `JoinLabelNamespaceNotAllowed` | Warning | Pod | The named vnet exists, but its `spec.allowedNamespaces` does not permit the pod's namespace. |

Pods in `kube-vnet/disabled=true` (or `--disabled-namespaces`) namespaces are skipped — explicit opt-out trumps diagnostic noise.

### VirtualNetworkBinding event reasons

A `VirtualNetworkBinding`'s `Ready` condition uses these reasons (constants in `internal/controller/virtualnetworkbinding_controller.go`):

| Reason | Status | Meaning |
|---|---|---|
| `PodsAttached` | True | Selector matched at least one pod; binding-driven policy is in place. |
| `NoPodsMatch` | False | Selector valid but matched zero pods in the binding's namespace. |
| `VirtualNetworkNotFound` | False | `spec.virtualNetworkRef` does not resolve. |
| `NamespaceNotAllowed` | False | The target vnet's `spec.allowedNamespaces` does not permit the binding's namespace. |
| `NamespaceExcluded` | False | The binding's namespace has `kube-vnet/disabled=true` or is in `--disabled-namespaces`. |
| `UnknownDirection` | False | `spec.direction` is not one of the recognized values. |
| `InvalidSelector` | False | `spec.podSelector` cannot be parsed. |

### Inspect events

```bash
# Recent events on a specific vnet
kubectl describe vnet -n <ns> <name>

# All events for a kind
kubectl get events -A --field-selector involvedObject.kind=VirtualNetwork \
  --sort-by='.lastTimestamp' | tail -20

# Just the Warning ones across the cluster
kubectl get events -A --field-selector type=Warning,involvedObject.kind=VirtualNetwork \
  --sort-by='.lastTimestamp' | tail -20

# All PolicyRestored events (drift signal)
kubectl get events -A --field-selector reason=PolicyRestored \
  --sort-by='.lastTimestamp'
```

### Forward events to your aggregator

If you run `kube-state-metrics` with the events collector enabled, every Event becomes a `kube_events` series:

```promql
sum by (namespace, reason) (rate(kube_events{involvedObject_kind="VirtualNetwork"}[5m]))
```

Most event aggregators (Datadog, Splunk, Elastic) consume Kubernetes Events directly via the apiserver — no extra config needed beyond their normal cluster integration.

---

## Status conditions (recap from `api.md`)

Each `VirtualNetwork.status.conditions` carries `Ready` and `Degraded`. Full reason taxonomy in [`api.md`](api.md). Brief recap:

| Condition | Status | Common reasons |
|---|---|---|
| `Ready` | True | `PoliciesGenerated`, `NoMembers` |
| `Ready` | False | `ApplyFailed`, `InvalidName`, `HomeNamespaceExcluded`, `NameCollision` |
| `Degraded` | False | `NoIssues` |
| `Degraded` | True | `InvalidJoiners`, `UnknownDirection`, `ConflictingDirections`, `InvalidName`, `HomeNamespaceExcluded`, `NameCollision` |

`kubectl wait --for=condition=Ready vnet/<name> -n <ns>` works because of this standard pattern.
