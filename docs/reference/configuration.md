# Configuration reference

Every operator flag, every Helm value, every environment variable.

For installation, see [`../install.md`](../install.md). For the reasoning behind defaults, see the linked ADRs.

---

## Operator command-line flags

The operator binary (`/manager` in the container) accepts these flags. They map 1:1 to the Helm `operator.*` values.

| Flag | Type | Default | Description |
|---|---|---|---|
| `--metrics-bind-address` | string | `:8080` | Address the Prometheus metrics endpoint listens on. Use `:0` to disable. |
| `--health-probe-bind-address` | string | `:8081` | Address the `/healthz` and `/readyz` endpoints listen on. |
| `--leader-elect` | bool | `false` (binary) / `true` (chart) | Enable leader election. Required for safe multi-replica HA. The chart sets it on by default; the bare binary defaults to off so local `make run` doesn't need a leader-election RBAC. |
| `--label-prefix` | string | `kube-vnet/` | Label-key prefix for join labels. Must end with `/`. Changing this is rare; mostly useful if you already have an unrelated `kube-vnet/` namespace in your cluster's labels. |
| `--disabled-namespaces` | string (comma-separated) | `kube-system,kube-public,kube-node-lease` | Namespaces the operator never touches (no baseline, no system vnets, no resolution stamping). The operator's own namespace (read from the `POD_NAMESPACE` env via the downward API) is always added implicitly. Mirrors the per-namespace `kube-vnet/disabled=true` annotation. See [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md), [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md). |
| `--version` | bool | `false` | Print version info and exit. |

Plus the standard `--zap-*` flags from `sigs.k8s.io/controller-runtime/pkg/log/zap` (log level, format, etc.).

### Inheritance order for vnet membership (per pod)

Per [ADR 0031](../adr/0031-baseline-tier-resolution.md), the resolution controller computes effective `(vnet, direction)` per pod by walking three tiers from lowest to highest priority:

1. `ClusterVirtualNetworkBaseline` named `default` (cluster-scoped singleton; chart-seeded from `operator.clusterBaseline`).
2. `VirtualNetworkBaseline` named `default` in the pod's namespace (singleton).
3. **Pod tier**: `VirtualNetworkBinding` CRs that match the pod (must use a non-empty selector) plus the pod's own `kube-vnet/net.<vnet>=<direction>` labels. Sources within this tier intersect on conflict (fail-closed).

Cross-tier override-permission is encoded in the eight-value `Direction` enum: bare values (`both`, `ingress`, `egress`, `none`) are enforced — lower tiers cannot override; `default-*` variants (only valid at baseline tiers) are advisory. The reserved-name VAP from the previous PR pins the system-vnet names (`namespace`, `cluster`) for the operator's exclusive use.

`kube-vnet/disabled=true` and `--disabled-namespaces` membership override everything: the operator does nothing in those namespaces.

### Example: configuring the cluster baseline via Helm

The chart seeds the singleton `ClusterVirtualNetworkBaseline` named `default` from `operator.clusterBaseline`. When `create=true`, exactly one of `ingressIsolationLevel` (one of `pod` / `namespace` / `cluster`) or `memberships` (explicit map of `<vnet-key>: <direction>`) must be set.

```bash
# Same-NS reachable, cross-NS egress only (the historical "namespace" mode).
helm install ... --set operator.clusterBaseline.ingressIsolationLevel=namespace
```

---

## Environment variables

| Variable | Source | Purpose |
|---|---|---|
| `POD_NAMESPACE` | Kubernetes downward API (set by the Deployment) | The operator's own namespace. Always added to the operator-level exclusion list at startup. Used to scope the leader-election lease. |

The chart and the kustomize manifests both set `POD_NAMESPACE` via:

```yaml
env:
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
```

If you run the binary outside Kubernetes (`make run`), `POD_NAMESPACE` is unset and the implicit self-exclusion doesn't fire. That's fine for local development.

---

## Helm chart values

Mirror of `charts/kube-vnet/values.yaml`. Pass any of these via `--set <key>=<value>` or a values file.

### `image.*`

| Key | Type | Default | Description |
|---|---|---|---|
| `image.repository` | string | `ghcr.io/lhns/kube-vnet` | Operator image repository. |
| `image.tag` | string | `""` (uses `Chart.appVersion`) | Operator image tag. |
| `image.pullPolicy` | string | `IfNotPresent` | Standard Kubernetes pull policy. |
| `image.pullSecrets` | `[]LocalObjectReference` | `[]` | Image-pull secrets in the release namespace. |

### `replicaCount`

| Key | Type | Default | Description |
|---|---|---|---|
| `replicaCount` | int | `1` | Operator replicas. Scale to 2+ for HA across nodes; pair with anti-affinity. Leader election is on by default, so multi-replica is safe. |

### `operator.*` (one-to-one with the binary's flags)

| Key | Type | Default | Description |
|---|---|---|---|
| `operator.labelPrefix` | string | `kube-vnet/` | → `--label-prefix`. |
| `operator.disabledNamespaces` | `[]string` | `[kube-system, kube-public, kube-node-lease]` | → `--disabled-namespaces`. The operator's own namespace is added implicitly via `POD_NAMESPACE`. Mirrors the per-namespace `kube-vnet/disabled=true` annotation. |
| `operator.clusterBaseline.create` | bool | `true` | Whether the chart seeds the singleton `ClusterVirtualNetworkBaseline` named `default`. Set to `false` to manage that CR outside Helm. ADR 0031. |
| `operator.clusterBaseline.ingressIsolationLevel` | string (`pod` / `namespace` / `cluster`) | `""` (REQUIRED if `create=true` and `memberships` unset) | Preset that maps to a system-vnet membership pair. Mutually exclusive with `memberships`. |
| `operator.clusterBaseline.memberships` | map `<vnet-key>: <direction>` | `null` | Explicit override map. Keys: bare for system vnets (resolves to release-namespace), `<namespace>.<name>` for user vnets. Mutually exclusive with `ingressIsolationLevel`. |
| `operator.leaderElect` | bool | `true` | → `--leader-elect`. Recommended on; harmless with one replica and required for safe multi-replica HA. |
| `operator.metricsBindAddress` | string | `:8080` | → `--metrics-bind-address`. |
| `operator.healthProbeBindAddress` | string | `:8081` | → `--health-probe-bind-address`. |

### `rbac.*` (end-user RBAC)

| Key | Type | Default | Description |
|---|---|---|---|
| `rbac.aggregate` | bool | `true` | Ship aggregated end-user ClusterRoles for the namespace-scoped CRDs (auto-merge into upstream `admin`/`edit`/`view`) plus an unbound editor + viewer pair for `ClusterVirtualNetworkBaseline`. Set `false` to skip and manage RBAC outside Helm. |

When `rbac.aggregate=true`, the chart emits eight ClusterRoles:

| ClusterRole | Aggregates into | Grants |
|---|---|---|
| `<release>-virtualnetworks-editor` | `admin`, `edit` | CRUD on `virtualnetworks` (+ `/status`) |
| `<release>-virtualnetworks-viewer` | `view` | read on `virtualnetworks` |
| `<release>-virtualnetworkbindings-editor` | `admin`, `edit` | CRUD on `virtualnetworkbindings` (+ `/status`) |
| `<release>-virtualnetworkbindings-viewer` | `view` | read on `virtualnetworkbindings` |
| `<release>-virtualnetworkbaselines-editor` | `admin`, `edit` | CRUD on `virtualnetworkbaselines` (+ `/status`) |
| `<release>-virtualnetworkbaselines-viewer` | `view` | read on `virtualnetworkbaselines` |
| `<release>-clustervirtualnetworkbaselines-editor` | (none — unbound) | CRUD on `clustervirtualnetworkbaselines` (+ `/status`); cluster-admin binds explicitly to delegate |
| `<release>-clustervirtualnetworkbaselines-viewer` | (none — unbound) | read on `clustervirtualnetworkbaselines`; cluster-admin binds explicitly |

See [`security.md`](../security.md#who-can-write-what) for the trust-model rationale.

### Pod-level scheduling

| Key | Type | Default | Description |
|---|---|---|---|
| `nodeSelector` | object | `{}` | Standard Pod `nodeSelector`. |
| `tolerations` | array | `[]` | Standard Pod `tolerations`. |
| `affinity` | object | `{}` | Standard Pod `affinity`. Use this for anti-affinity in HA. |
| `priorityClassName` | string | `""` | Standard `priorityClassName`. |

### Security context

| Key | Type | Default | Description |
|---|---|---|---|
| `podSecurityContext` | object | `{ runAsNonRoot: true, seccompProfile: { type: RuntimeDefault } }` | Pod-level security context. |
| `securityContext` | object | `{ allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }` | Container-level security context. |

### Resources & probes

| Key | Type | Default | Description |
|---|---|---|---|
| `resources` | object | `{ limits: { cpu: 500m, memory: 256Mi }, requests: { cpu: 50m, memory: 64Mi } }` | Standard CPU/memory requests and limits. See [`../operations.md`](../operations.md#resource-sizing) for sizing guidance. |
| `livenessProbe.initialDelaySeconds` | int | `15` | Standard. |
| `livenessProbe.periodSeconds` | int | `20` | Standard. |
| `readinessProbe.initialDelaySeconds` | int | `5` | Standard. |
| `readinessProbe.periodSeconds` | int | `10` | Standard. |

### Optional metrics surface

| Key | Type | Default | Description |
|---|---|---|---|
| `metricsService.enabled` | bool | `false` | Create a `ClusterIP` Service exposing `:8080`. Useful if Prometheus scrapes via Service rather than Pod. |
| `metricsService.port` | int | `8080` | Service port. |
| `metricsService.type` | string | `ClusterIP` | Service type. |
| `podMonitor.enabled` | bool | `false` | Create a `monitoring.coreos.com/v1 PodMonitor` (requires the Prometheus operator). |
| `podMonitor.interval` | string | `30s` | Scrape interval. |
| `podMonitor.scrapeTimeout` | string | `10s` | Scrape timeout. |
| `podMonitor.labels` | object | `{}` | Extra labels on the PodMonitor (used for Prometheus-operator selector matching). |

### Misc

| Key | Type | Default | Description |
|---|---|---|---|
| `commonLabels` | object | `{}` | Extra labels applied to every templated resource. |
| `podAnnotations` | object | `{}` | Extra annotations on the operator Pod. |
| `terminationGracePeriodSeconds` | int | `10` | Standard `terminationGracePeriodSeconds`. |
| `nameOverride` | string | `""` | Overrides the chart's name in resource names. Rarely useful. |
| `fullnameOverride` | string | `""` | Overrides the chart's full name. |

---

## Defaults rationale

A few of the defaults aren't obvious; here's why.

### Why `replicaCount: 1`?

kube-vnet is a control-plane operator, not a data-plane one. Existing `NetworkPolicy` keeps working while the operator is down; only change-propagation pauses. A single replica is enough for typical clusters; scaling to 2 is for node-failure resilience, not throughput. See [`../operations.md`](../operations.md#deployment-topology).

### Why `--leader-elect=true` in the chart but `false` in the binary?

The chart matches "what cert-manager / Cilium operator / Flux do" — leader election on by default so scaling is one line. The binary defaults to off so local `make run` doesn't need leader-election RBAC.

### Why does `disabledNamespaces` default to the control-plane namespaces?

`kube-system`, `kube-public`, `kube-node-lease` — Kubernetes control-plane namespaces. The operator stays out of those entirely (no baseline, no system vnets, no resolution stamping) so it can't accidentally break CoreDNS, the metrics server, or the apiserver aggregator. To enroll a system-namespace pod in a vnet, remove the namespace from this list explicitly. See [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md), [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md).

The operator's own namespace is implicitly added to `disabledNamespaces` so that a misconfigured `allowedNamespaces.all: true` vnet can't accidentally lock the operator out of itself.

### Why such small `resources.requests`?

Idle operator footprint is tiny — a few MB resident, near-zero CPU. The requests are sized so the operator schedules anywhere; the limits give it headroom for a reconcile burst. See [`../operations.md` § Resource sizing](../operations.md#resource-sizing) for when to bump.
