# Metrics and Events reference

Every Prometheus metric the operator exposes, every Kubernetes Event reason it emits, and sample queries for each.

For where to scrape, see [`operations.md`](../guides/operations.md#observability).

---

## Metrics

The operator exposes Prometheus text format on `:8080/metrics`. Six domain-specific metrics on top of the controller-runtime defaults (`workqueue_*`, `controller_runtime_*`, `rest_client_*`, Go runtime — documented [upstream](https://github.com/kubernetes-sigs/controller-runtime/tree/main/pkg/metrics)).

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
| **Description** | Number of `NetworkPolicy` resources currently labeled `kube-vnet.system/managed-by=kube-vnet` (baselines, membership and auto-allow policies). |
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
| **Description** | Distinct member pods per VirtualNetwork, across all member namespaces. |
| **When it changes** | At the end of each successful `Reconcile`. Cleared on vnet deletion via `clearMembers`. |

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

Events are best-effort notifications with the apiserver's default TTL (1 hour), not an audit log. Status conditions are the source of truth for current state.

Every reason the operator emits:

| Reason | Type | Emitted on | Source (`From`) | When |
|---|---|---|---|---|
| `Ready` | Normal | VirtualNetwork | `kube-vnet` | `Ready` condition transitions to True. The event message is the condition message. |
| `NotReady` | Warning | VirtualNetwork | `kube-vnet` | `Ready` condition transitions to False. |
| `Degraded` | Warning | VirtualNetwork | `kube-vnet` | `Degraded` condition transitions to True. |
| `Recovered` | Normal | VirtualNetwork | `kube-vnet` | `Degraded` condition transitions to False. |
| `ApplyFailed` | Warning | VirtualNetwork | `kube-vnet` | A membership `NetworkPolicy` apply returned an error. Fires at the failure site, independent of condition transitions; the message names the policy and the apiserver error. |
| `PolicyRestored` | Warning | VirtualNetwork | `kube-vnet` | The operator re-created a membership `NetworkPolicy` that was absent immediately before its apply, i.e. an out-of-band deletion was reverted. See [ADR 0019](../adr/0019-baseline-durability.md). |
| `VirtualNetworkNotJoinable` | Warning | the Pod, `VirtualNetworkBinding`, `VirtualNetworkBaseline` or `ClusterVirtualNetworkBaseline` that declared the membership | `kube-vnet-resolution` | A referenced vnet can't be joined: it doesn't exist at the resolved namespace (a bare `kube-vnet/net.<X>` label with no local vnet `<X>` gets a hint to use the prefixed form), or its `spec.allowedNamespaces` doesn't permit the pod's namespace. See [ADR 0027](../adr/0027-pod-scoped-join-label-events.md) and [ADR 0043](../adr/0043-virtualnetworkref-namespace-inferred-or-honored.md). |
| `InvalidJoinLabelDirection` | Warning | Pod | `kube-vnet-resolution` | A `kube-vnet/net.*` label has a value other than `both`, `ingress`, `egress`, `none`. The label is ignored until fixed. Mostly relevant where the join-label `ValidatingAdmissionPolicy` is absent (Kubernetes < 1.30). |
| `Pending` | Warning | Service | `kube-vnet-external-allow` / `kube-vnet-apiserver-reachable` | An auto-allow policy is held back because a named `targetPort` has no backing pod with a matching `containerPort` name yet. Retried every 30s. |
| `Skipped` | Normal | Service | `kube-vnet-external-allow` | An externally exposed Service has no `spec.selector`, so no `ext.svc` policy can be derived. |

The operator does not emit Events for pods in disabled namespaces. There are no binding events: a `VirtualNetworkBinding` reports its state only through its `Ready` condition ([reasons](api.md#ready-condition-1)).

Status-condition reasons for all CRDs are in [`api.md`](api.md); the constants are the `Reason*` blocks in `internal/controller/virtualnetwork_controller.go` and `virtualnetworkbinding_controller.go`.

### Inspect events

```bash
# Recent events on a specific vnet
kubectl describe vnet -n <ns> <name>

# Warning events on VirtualNetworks across the cluster
kubectl get events -A --field-selector type=Warning,involvedObject.kind=VirtualNetwork \
  --sort-by='.lastTimestamp' | tail -20

# Memberships the operator could not honor
kubectl get events -A --field-selector reason=VirtualNetworkNotJoinable

# All PolicyRestored events (drift signal)
kubectl get events -A --field-selector reason=PolicyRestored \
  --sort-by='.lastTimestamp'
```

### Forward events to your aggregator

If you run `kube-state-metrics` with the events collector enabled, every Event becomes a `kube_events` series:

```promql
sum by (namespace, reason) (rate(kube_events{involvedObject_kind="VirtualNetwork"}[5m]))
```

Most event aggregators (Datadog, Splunk, Elastic) consume Kubernetes Events directly via the apiserver.

---

## Diagnosing reconcile churn

If the operator's CPU looks high, the question is almost always *"which controller is reconciling, and why"*. kube-vnet's own `kube_vnet_reconciliations_total` is labelled by `result` only, so it cannot attribute churn — use controller-runtime's built-in metrics, which are exported on the same `/metrics` endpoint. Every controller is named, so the `controller` label is meaningful.

```promql
# Which controller is spinning? (names: virtualnetwork, resolution,
# host-port, external-allow, apiserver-reachable,
# namespace, system-vnet, virtualnetworkbinding)
sum by (controller) (rate(controller_runtime_reconcile_total[5m]))

# Is it apiserver traffic, and is it reads or writes? A high PUT rate on a
# steady cluster means something is writing status in a loop.
sum by (verb) (rate(rest_client_requests_total[5m]))

# Queue pressure — adds that never settle indicate a self-feeding loop.
sum by (name) (rate(workqueue_adds_total[5m]))
workqueue_depth
```

**What healthy looks like.** On an idle cluster, reconcile rate should be near zero apart from the 10-minute periodic resync per VirtualNetwork. A sustained non-zero rate with no one changing anything means an event source is firing on data the operator doesn't care about — historically the two causes were an unconditional status write (each write re-triggering its own watch) and pod predicates that fired on status heartbeats rather than label changes. Both are fixed, and both are covered by regression tests, but the same shape can reappear whenever a watch is added without a predicate.

**When you see churn:** check `workqueue_adds_total` first to find the busy controller, then look at what that controller watches. A controller whose adds greatly exceed the rate of real object changes is being triggered by something it should be filtering out.
