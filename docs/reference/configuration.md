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
| `--excluded-namespaces` | string (comma-separated) | `kube-system,kube-public,kube-node-lease` | Namespaces the operator never touches. The operator's own namespace (read from the `POD_NAMESPACE` env via the downward API) is always added implicitly. See [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md). |
| `--ingress-isolation` | string enum | `none` | Cluster-wide default ingress-isolation mode. One of `none`, `namespace`, `pod`. Per-namespace `kube-vnet/ingress-isolation` annotations and the per-mode override lists below take precedence. See [ADR 0024](../adr/0024-ingress-isolation-mode-and-overrides.md) and [ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md). |
| `--ingress-isolation-none` | string (CSV) | `""` | Namespaces forced to `ingress-isolation=none` regardless of the cluster-wide default. |
| `--ingress-isolation-namespace` | string (CSV) | `""` | Namespaces forced to `ingress-isolation=namespace`. |
| `--ingress-isolation-pod` | string (CSV) | `""` | Namespaces forced to `ingress-isolation=pod`. |
| `--default-deny-everywhere` | bool | `false` | **Deprecated.** Aliased to `--ingress-isolation=pod` when the new flag is at its default. Logs a deprecation warning at startup. Will be removed in a future release. See [ADR 0024](../adr/0024-ingress-isolation-mode-and-overrides.md). |
| `--version` | bool | `false` | Print version info and exit. |

Plus the standard `--zap-*` flags from `sigs.k8s.io/controller-runtime/pkg/log/zap` (log level, format, etc.).

### Resolution order (per namespace)

1. The namespace's `kube-vnet/ingress-isolation` annotation, if set to a recognized value (`none`, `namespace`, `pod`).
2. The matching per-mode override list (`--ingress-isolation-{none,namespace,pod}`).
3. The cluster-wide default `--ingress-isolation`.

A namespace listed in two override lists is a startup configuration error; the operator refuses to start.

`kube-vnet/disabled=true` and `--excluded-namespaces` membership override everything: the operator does nothing in those namespaces, regardless of `ingress-isolation` config.

### Example: cluster-wide ingress-deny posture

```bash
manager \
  --leader-elect \
  --metrics-bind-address=:8080 \
  --health-probe-bind-address=:8081 \
  --ingress-isolation=pod \
  --ingress-isolation-none=legacy,sandbox \
  --excluded-namespaces=kube-system,kube-public,kube-node-lease,my-legacy-ns
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
| `operator.excludedNamespaces` | `[]string` | `[kube-system, kube-public, kube-node-lease]` | → `--excluded-namespaces` (the chart joins them with commas). |
| `operator.ingressIsolation.mode` | string enum | `none` | → `--ingress-isolation`. One of `none`, `namespace`, `pod`. |
| `operator.ingressIsolation.forceNone` | `[]string` | `[]` | → `--ingress-isolation-none`. Namespaces forced to `none`. |
| `operator.ingressIsolation.forceNamespace` | `[]string` | `[]` | → `--ingress-isolation-namespace`. |
| `operator.ingressIsolation.forcePod` | `[]string` | `[]` | → `--ingress-isolation-pod`. |
| `operator.defaultDenyEverywhere` | bool | `false` | **Deprecated.** Aliased to `operator.ingressIsolation.mode=pod` when the new value is at its default. Logs a deprecation warning at startup. |
| `operator.leaderElect` | bool | `true` | → `--leader-elect`. Recommended on; harmless with one replica and required for safe multi-replica HA. |
| `operator.metricsBindAddress` | string | `:8080` | → `--metrics-bind-address`. |
| `operator.healthProbeBindAddress` | string | `:8081` | → `--health-probe-bind-address`. |

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

### Why `--ingress-isolation=none` by default?

Backward compatibility. Turning the cluster-wide default to `pod` against an existing cluster would impose ingress-deny on every workload that's not yet using vnets. That's a bigger commitment than installing the operator; opt-in. Per-namespace migrations can use the `kube-vnet/ingress-isolation` annotation or the `--ingress-isolation-{none,namespace,pod}` override lists. See [ADR 0024](../adr/0024-ingress-isolation-mode-and-overrides.md).

### Why these `--excluded-namespaces` defaults?

`kube-system`, `kube-public`, `kube-node-lease` — Kubernetes control-plane namespaces. Installing the deny baseline in `kube-system` would block CoreDNS's own egress to the apiserver, breaking cluster-wide DNS. See [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md).

The operator's own namespace is implicitly added so that a misconfigured `allowedNamespaces.all: true` vnet can't accidentally lock the operator out of itself.

### Why such small `resources.requests`?

Idle operator footprint is tiny — a few MB resident, near-zero CPU. The requests are sized so the operator schedules anywhere; the limits give it headroom for a reconcile burst. See [`../operations.md` § Resource sizing](../operations.md#resource-sizing) for when to bump.
