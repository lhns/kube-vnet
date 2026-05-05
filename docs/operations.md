# Operations

Running kube-vnet in production. Topology, sizing, monitoring, alerts.

For the underlying mechanism (reconciler internals, baseline lifecycle), see [`architecture.md`](architecture.md). For the full metric/event surface, see [`reference/metrics-and-events.md`](reference/metrics-and-events.md).

---

## Deployment topology

### Single replica + leader election (default)

The chart and the rendered manifests both ship with `replicas: 1` and `--leader-elect` on. This is what almost every Kubernetes operator does — it's the right default:

- **Single replica** is enough. kube-vnet is a control-plane operator, not a data-plane one. While the operator is down, existing `NetworkPolicy` resources keep working (the apiserver serves them and the CNI enforces them). Only *change propagation* pauses.
- **Leader election** is on so that scaling to two or more replicas is a one-line change rather than a flag flip + RBAC re-apply. The lease lives in the operator's own namespace as a `coordination.k8s.io/v1 Lease` named `kube-vnet.lhns.de`.

Failure-mode characteristics:

| Failure | Recovery time |
|---|---|
| Operator container crash (OOM, panic) | ~5–10s — kubelet restarts the container; the new container picks up the lease. |
| Operator pod evicted (node pressure) | Reschedule + start, typically tens of seconds. |
| Node holding the operator dies | Default node-eviction timeout (40s) + scheduler latency + container start; can be a few minutes with one replica. With two replicas across nodes + leader election, ~15s. |

### HA: two replicas across nodes

Set `replicaCount: 2` in the Helm values. Pair it with anti-affinity so the replicas don't end up on the same node:

```yaml
# values.yaml
replicaCount: 2
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: kube-vnet
          topologyKey: kubernetes.io/hostname
```

With two replicas and leader election, a node failure that takes out the leader causes failover within ~15 seconds (lease duration + election jitter), instead of the multi-minute reschedule.

This is purely a failure-mode tradeoff — see "When the operator is down" below — not a throughput consideration. kube-vnet's reconcile rate is bounded by `VirtualNetwork` count, which is small in practice.

### When the operator is down

What happens to your cluster while no replica is running:

- **Existing `NetworkPolicy` resources stay enforced.** The apiserver continues serving them; the CNI continues dropping packets. Pods that *were* isolated remain isolated; pods that *were* allowed remain allowed.
- **Membership changes don't propagate.** A new pod with a join label won't show up in the corresponding vnet's `status.members` until the operator returns. Its connectivity, however, is governed by whatever NetworkPolicy is already in the namespace.
- **VirtualNetwork resources can still be created/edited/deleted.** The apiserver accepts them; the operator just won't act on them until it's back.
- **Drift correction pauses.** A user deleting an operator-managed `NetworkPolicy` while the operator is down won't trigger an immediate restore. The policy returns on the next reconcile after the operator comes back. See [`security.md`](security.md) for what this means for the threat model.

---

## Resource sizing

Defaults from the chart:

```yaml
resources:
  limits:   { cpu: 500m, memory: 256Mi }
  requests: { cpu: 50m,  memory: 64Mi }
```

These work for typical clusters. The operator's footprint scales with:

- **Number of `VirtualNetwork` resources** — each gets one informer entry, occasional reconciles. Negligible per-vnet.
- **Number of pods carrying `kube-vnet/net.*` labels** — each fires a watch event. The label-prefix predicate ensures pods *without* those labels never enter the work queue.
- **Number of operator-managed `NetworkPolicy` resources** — also gets an informer entry, drift-correction events.

Practical bumps:

- ~1000 vnets and ~10000 labeled pods → bump memory to `512Mi`, leave CPU.
- > 10k vnets → start measuring. The reconciler is O(N) per vnet event in member discovery (one cluster-wide pod list per reconcile); at very high vnet counts the listing cost dominates and an indexed cache becomes worth doing.

Watch the metrics (next section) before tuning.

---

## Leader election semantics

Implemented by `controller-runtime` against a `coordination.k8s.io/v1 Lease`:

- **Lease object**: `kube-vnet.lhns.de` in the operator's own namespace.
- **RBAC**: a namespace-scoped `Role` (in the operator's namespace) granting `coordination.k8s.io/leases` CRUD.
- **Lease duration / renew deadline / retry**: controller-runtime defaults (~15s / ~10s / ~2s).

A non-leader replica blocks at `Manager.Start()` until it acquires the lease. Only the leader's reconcilers are running.

Inspect the current leader:

```bash
kubectl get lease -n kube-vnet-system kube-vnet.lhns.de -o yaml
```

The `holderIdentity` field names the pod that currently holds the lock; `renewTime` updates roughly every 2s while it's healthy.

---

## Observability

### Metrics

The operator exposes `:8080/metrics` (Prometheus text format). Six domain-specific metrics on top of the controller-runtime defaults — see [`reference/metrics-and-events.md`](reference/metrics-and-events.md) for every metric name, type, and label.

By default the metrics endpoint is **not exposed via a Service**. Enable it (or use a `PodMonitor` if you have the Prometheus operator):

```yaml
# values.yaml
metricsService:
  enabled: true   # creates a ClusterIP Service on :8080

# or, for the Prometheus operator
podMonitor:
  enabled: true
  interval: 30s
```

Sample scrape config (no Prometheus operator):

```yaml
scrape_configs:
  - job_name: kube-vnet
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [kube-vnet-system]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_name]
        action: keep
        regex: kube-vnet
      - source_labels: [__meta_kubernetes_pod_container_port_name]
        action: keep
        regex: metrics
```

### Sample alerting rules

```yaml
groups:
  - name: kube-vnet
    rules:
      - alert: KubeVnetApplyErrors
        expr: |
          increase(kube_vnet_apply_errors_total[5m]) > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "kube-vnet failed to apply NetworkPolicies"
          description: "{{ $value }} apply errors in the last 5m. Check operator logs."

      - alert: KubeVnetReconcileErrorRate
        expr: |
          sum(rate(kube_vnet_reconciliations_total{result="error"}[5m]))
            / sum(rate(kube_vnet_reconciliations_total[5m])) > 0.1
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "kube-vnet reconcile error rate above 10%"

      - alert: KubeVnetSlowReconcile
        expr: |
          histogram_quantile(0.95, rate(kube_vnet_reconcile_duration_seconds_bucket[5m])) > 5
        for: 15m
        labels:
          severity: info
        annotations:
          summary: "kube-vnet p95 reconcile latency > 5s"
          description: "Likely high vnet count or apiserver pressure."

      - alert: KubeVnetPolicyRestoredRepeatedly
        expr: |
          sum by (namespace) (
            increase(kube_events{reason="PolicyRestored"}[15m])
          ) > 5
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "kube-vnet keeps restoring deleted NetworkPolicies in {{ $labels.namespace }}"
          description: |
            Something is repeatedly deleting operator-managed policies and the
            operator is restoring them. Could be a misbehaving controller, a
            CI loop, or an attempted bypass — investigate.
```

(The `kube_events` series above is from `kube-state-metrics`. If you're not running it, watch `kubectl get events --field-selector reason=PolicyRestored -A` instead.)

### Logs

Structured JSON via zap (controller-runtime default). Stream:

```bash
kubectl logs -n kube-vnet-system deploy/kube-vnet-controller -f
```

Per-reconcile log lines look like:

```json
{"level":"info","ts":"...","msg":"...","controller":"virtualnetwork","controllerKind":"VirtualNetwork","VirtualNetwork":{"name":"payments","namespace":"platform"}}
```

Errors include a stack trace. Two recurring messages are benign and don't indicate a problem:

- `"Operation cannot be fulfilled on virtualnetworks.kube-vnet.lhns.de \"X\": the object has been modified; please apply your changes to the latest version and try again"` — optimistic-concurrency conflict during a status update; the controller retries.
- `"is forbidden: unable to create new content in namespace X because it is being terminated"` — a reconcile fired between `kubectl delete namespace` and the namespace finalizer completing. The reconciler will see the namespace gone on the next attempt.

### Kubernetes Events

Each VirtualNetwork emits Events on condition transitions and on apply errors / restored policies. Inspect:

```bash
kubectl describe vnet -n <ns> <name>
# or
kubectl get events -n <ns> --field-selector involvedObject.kind=VirtualNetwork
```

Full event list in [`reference/metrics-and-events.md`](reference/metrics-and-events.md).

Events have a default TTL of 1 hour (apiserver-managed) — they're a notification mechanism, not a durable audit log. Conditions on the VirtualNetwork status are the source of truth for current state.

---

## Operational playbooks

### "I just installed kube-vnet — why aren't my non-vnet pods isolated?"

By default they are: every managed namespace gets a deny-all baseline. If pods can still reach each other, the most likely cause is that they're members of the same vnet (which adds an allow rule). Check `kubectl get netpol -A -l kube-vnet/role=baseline` to confirm the baseline is present in your namespace.

If you want to *open up* a namespace, annotate it `kube-vnet/disabled: "true"` (the operator stays out entirely there) or add it to `--disabled-namespaces` / `operator.disabledNamespaces`.

### "I want to know if kube-vnet is healthy"

```bash
# Operator running and Available
kubectl get deploy -n kube-vnet-system kube-vnet-controller

# Lease being renewed
kubectl get lease -n kube-vnet-system kube-vnet.lhns.de \
  -o jsonpath='{.spec.holderIdentity} {.spec.renewTime}{"\n"}'

# All vnets are Ready
kubectl get vnet -A
```

If `READY` for any vnet is False, see [`troubleshooting.md`](troubleshooting.md).

### "I'm rolling out a new operator version"

Helm:

```bash
helm upgrade kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version <new> --namespace kube-vnet-system --reuse-values
```

The Deployment uses `RollingUpdate` (Kubernetes default). With one replica and leader election, you'll see a brief gap (~10–20s) where neither replica is leader. Existing policies stay enforced throughout. With two replicas, failover is faster.

CRD changes are not applied by `helm upgrade` — see [`install.md`](install.md) for how to apply CRD updates explicitly.

### "I want to roll out cluster-wide isolation"

Per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md) the deny-all baseline applies to every managed namespace by default — if you've already installed kube-vnet, isolation is in place. Migration risk is real on existing clusters: workloads that previously relied on default-allow ingress will break the moment kube-vnet starts.

Recommended rollout:

1. *Before* installing, mark every namespace that isn't ready to be isolated:
   ```bash
   kubectl annotate namespace <name> kube-vnet/disabled=true
   ```
2. Install the chart. The deny-all baseline lands only in non-disabled namespaces.
3. Migrate namespaces to vnets one at a time. Add `VirtualNetwork` + `kube-vnet/net.<vnet>` labels (or `VirtualNetworkBinding`) so the workloads that need to reach each other are vnet members.
4. Remove the `kube-vnet/disabled` annotation namespace by namespace. The deny-all baseline applies; vnet membership grants the allows.

### "An auditor is asking what kube-vnet does in `kube-system`"

Nothing. `kube-system`, `kube-public`, and `kube-node-lease` are in the chart's default `operator.disabledNamespaces`, so the operator stays out of them entirely: no baseline, no system vnets, no resolution stamping, no eligibility as a peer for foreign-NS vnets. To enroll a system-namespace pod in a vnet, remove the namespace from `disabledNamespaces` explicitly. See [`security.md`](security.md) for the full RBAC inventory.

### "I'm upgrading from a release with the old config-key names"

[ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md) removed the `--ingress-isolation*` flag family and the `kube-vnet/ingress-isolation` annotation. Read these before `helm upgrade`.

**`operator.ingressIsolation.mode` and the `--ingress-isolation*` flags are gone.** The baseline is uniformly deny-all minus `--elide-baseline-for` exemptions; there's no per-namespace mode anymore. To configure the cluster-wide default posture, set `operator.clusterBaseline.ingressIsolationLevel` (one of `pod` / `namespace` / `cluster`) on the chart — see [ADR 0031](adr/0031-baseline-tier-resolution.md). The chart fails fast at install time if neither `ingressIsolationLevel` nor `memberships` is set when `create=true`; pick deliberately.

**System namespaces are disabled by default again.** `operator.disabledNamespaces` defaults to `[kube-system, kube-public, kube-node-lease]`; the operator stays out of those entirely.

**Renamed/removed values — old names no longer accepted.** Rename in your values file and CI manifests *before* you upgrade:

| Old (removed) | New |
|---|---|
| `operator.excludedNamespaces` | `operator.disabledNamespaces` |
| CLI `--excluded-namespaces` | CLI `--disabled-namespaces` |
| `operator.ingressIsolation.mode` | `operator.clusterBaseline.ingressIsolationLevel` (different semantic; see [`concepts.md`](concepts.md) and [ADR 0031](adr/0031-baseline-tier-resolution.md)) |
| `operator.ingressIsolation.namespaceOverrides.{none,namespace,pod}` | (removed; use the per-namespace `kube-vnet/disabled` annotation, a per-NS `VirtualNetworkBaseline`, or per-pod vnet membership) |
| `operator.ingressIsolation.force{None,Namespace,Pod}` | (removed; same migration as above) |
| CLI `--ingress-isolation` and `--ingress-isolation-{none,namespace,pod}` | (removed) |
| CLI `--default-memberships` / `operator.defaultMemberships` | (removed in 0.4 ADR-0031 cleanup; use `operator.clusterBaseline.{ingressIsolationLevel, memberships}` instead) |
| `ClusterVirtualNetworkBinding` CRD | (removed in 0.4 ADR-0031 cleanup; broad-selector usage migrates to `ClusterVirtualNetworkBaseline`, narrow-selector to `VirtualNetworkBinding` in the target NS) |
| `VirtualNetworkBinding` with empty `podSelector` | (rejected at admission; namespace-wide defaults move to `VirtualNetworkBaseline`) |
| CLI `--default-deny-everywhere` and `operator.defaultDenyEverywhere` | (removed) |
| `kube-vnet/ingress-isolation` namespace annotation | (removed) |

Two related behavior reminders:

- **Egress is not restricted** by the baseline ([ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md)). If you need per-workload egress restriction, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` — see [`recipes.md`](recipes.md) and [`security.md`](security.md).
- **Vnet membership is the only ingress-allow mechanism**, including for "open up a namespace" cases. To allow same-NS ingress without joining a user vnet, set `operator.clusterBaseline.ingressIsolationLevel=namespace` (chart) or write an explicit `ClusterVirtualNetworkBaseline` with `namespace=default-both`.

### "Pods I expect to be isolated can talk to each other"

See [`troubleshooting.md`](troubleshooting.md) — most common causes: CNI doesn't enforce NetworkPolicy, baseline missing in one of the namespaces involved, label form wrong (bare vs prefixed).

---

## Resource overhead in the cluster

Per `VirtualNetwork`:

- 1 CRD object.
- Up to 1 `NetworkPolicy` per (namespace, direction class) with members. In the home namespace, when both bare and prefixed label forms are in use for a given direction class, two policies appear (one suffixed `-prefixed`). Empty member sets generate nothing.
- 1 extra `NetworkPolicy` per `VirtualNetworkBinding` attached to the vnet.
- Baselines are owned by the `NamespaceReconciler` (not by per-vnet reconciliation): 1 baseline per namespace whose resolved `ingress-isolation` mode is `namespace` or `pod`. Namespaces with mode `none` get no baseline.

Per labeled pod: 0 (the label is on a pod the user owns; the operator doesn't add anything to the pod itself).

The operator-managed `NetworkPolicy` resources are small (a few KB each). At cluster scale, even thousands of them are negligible compared to typical apiserver state.

---

## Label cardinality at scale

A pod's join labels are O(N) where N is the number of vnets it belongs to. For typical workloads (1–5 vnets per pod) this is trivial. For pods on many vnets (50+):

- Kubernetes' label storage handles it (no hard ceiling at this scale).
- The operator's pod-watch predicate scans labels per event — O(L) per pod event. L=50 is fine; L=500 would be measurable.
- Generated `NetworkPolicy` selectors are unaffected — the selector is `Exists` on a single key per vnet.

If you're approaching this scale, the e2e suite can validate it (the `TestE2E_*` tests are easy to extend with a high-cardinality case). It hasn't been benchmarked at 1000+ pods × 50+ vnets.
