# Configuration reference

Every operator flag, every Helm value, every environment variable.

For installation, see [`install.md`](../getting-started/install.md). For the reasoning behind defaults, see the linked ADRs.

---

## Operator command-line flags

Defined in `cmd/main.go`. The chart sets them from the `operator.*` and `webhook.*` values (see the `→` notes in the [value tables](#helm-chart-values)); `--webhook-port` and `--webhook-cert-dir` are not exposed by the chart.

| Flag | Type | Default | Description |
|---|---|---|---|
| `--metrics-bind-address` | string | `:8080` | Address the Prometheus metrics endpoint listens on. `0` disables it. |
| `--health-probe-bind-address` | string | `:8081` | Address the `/healthz` and `/readyz` endpoints listen on. |
| `--leader-elect` | bool | `false` (binary) / `true` (chart) | Enable leader election. Required for safe multi-replica HA. The chart sets it on by default; the bare binary defaults to off so local `make run` doesn't need a leader-election RBAC. |
| `--disabled-namespaces` | string (comma-separated) | `kube-system` | Namespaces the operator never touches (no baseline, no system vnets, no resolution stamping). The operator's own namespace (read from the `POD_NAMESPACE` env via the downward API) is always added implicitly — which is why the release namespace holds no per-namespace `namespace` system vnet. Mirrors the per-namespace `kube-vnet/disabled=true` annotation. See [ADR 0007](../adr/0007-operator-level-excluded-namespaces.md), [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md), [ADR 0042](../adr/0042-coredns-ingress-carveout-and-kube-system-enrollment.md). |
| `--apiserver-source-cidr` | string (CIDR) | `0.0.0.0/0` | Source CIDR allowed by the auto-emitted `kube-vnet.ext.apiserver.*` policies for Services the apiserver dials (admission webhooks, APIServices, CRD conversion webhooks). The default matches the no-NetworkPolicy baseline; tighten to your control-plane subnet when the pod network is externally reachable. Validated at startup — an unparseable CIDR exits 1. See [the auto-allow guide](../guides/auto-allow.md#apiserver-reachable-services-extapiserver) and [ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md). |
| `--webhook-enabled` | bool | `false` | Serve the pod-resolution admission webhooks (`/mutate-v1-pod`, `/validate-v1-pod`). Requires `POD_NAMESPACE` (startup fails without it) plus serving certificates and webhook configurations, which the chart installs when `webhook.enabled=true`. See [ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md). |
| `--webhook-port` | int | `9443` | Port of the webhook server. |
| `--webhook-cert-dir` | string | `/tmp/k8s-webhook-server/serving-certs` | Directory holding `tls.crt` and `tls.key`. |
| `--service-account-name` | string | `kube-vnet-controller` | The operator's own ServiceAccount name. The validating webhook exempts this identity so the operator's own pod patches are not judged as user writes. The chart passes its ServiceAccount name when `webhook.enabled=true`. |
| `--network-wait-beacons` | string (host:port) | `""` | The network beacons' headless Service. Non-empty enables the network wait: pods annotated `kube-vnet/network-max-wait` get the wait container. Requires `--webhook-enabled` and `--network-wait-image`. The chart sets both when `webhook.networkWait.enabled=true`. See [ADR 0045](../adr/0045-network-wait-for-opted-in-pods.md). |
| `--network-wait-image` | string | `""` | Image of the injected wait container: the operator image, which runs its `network-wait` subcommand. |
| `--version` | bool | `false` | Print version info and exit. |

Plus the standard `--zap-*` flags from `sigs.k8s.io/controller-runtime/pkg/log/zap` (log level, format, etc.).

How the three baseline tiers, bindings and labels combine into a pod's membership: [concepts § the `default-*` variants](../getting-started/concepts.md#the-default--variants-baseline-tiers-only) and [ADR 0031](../adr/0031-baseline-tier-resolution.md).

---

## Environment variables

| Variable | Source | Purpose |
|---|---|---|
| `POD_NAMESPACE` | Kubernetes downward API (set by the Deployment) | The operator's own namespace. Always added to the disabled-namespace list at startup, holds the `cluster` system vnet and the leader-election lease, and is required with `--webhook-enabled`. |

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

Mirror of `charts/kube-vnet/values.yaml`, plus `nameOverride`/`fullnameOverride`, which the templates read but `values.yaml` leaves unset. Pass any of these via `--set <key>=<value>` or a values file.

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

### `operator.*`

| Key | Type | Default | Description |
|---|---|---|---|
| `operator.disabledNamespaces` | `[]string` | `[kube-system]` | → `--disabled-namespaces`. The operator's own namespace is added implicitly via `POD_NAMESPACE`. Mirrors the per-namespace `kube-vnet/disabled=true` annotation. **Empty vs absent**: an empty list `[]` disables *nothing* (manage every namespace); `null` or omitting the key falls back to the binary default (`kube-system`). Removing `kube-system` here enrolls it — the chart then auto-renders the CoreDNS carve-out (see `dnsCarveout` and ADR 0042). |
| `operator.apiserverSourceCIDR` | string | `"0.0.0.0/0"` | → `--apiserver-source-cidr`. Source CIDR for the `kube-vnet.ext.apiserver.*` auto-allow policies (webhook / APIService backends the apiserver dials). See [auto-allow](../guides/auto-allow.md#apiserver-reachable-services-extapiserver). |
| `operator.clusterBaseline.create` | bool | `true` | Whether the chart seeds the singleton `ClusterVirtualNetworkBaseline` named `default`. It is re-rendered on every `helm upgrade`, overwriting hand edits. Set to `false` to manage that CR outside Helm. ADR 0031. |
| `operator.clusterBaseline.ingressIsolationLevel` | string (`pod` / `namespace` / `cluster`) | `""` (required if `create=true` and `memberships` unset) | Preset for the two system vnets: `cluster` → `namespace=default-both, cluster=default-both`; `namespace` → `namespace=default-both, cluster=default-egress`; `pod` → both `default-egress`. Mutually exclusive with `memberships`. |
| `operator.clusterBaseline.memberships` | map `<vnet-key>: <direction>` | `null` | Explicit override map. Keys: bare for the system vnets (rendered with no `namespace:` — `cluster` is the cluster-wide singleton, `namespace` resolves to each pod's *own* namespace; [ADR 0043](../adr/0043-virtualnetworkref-namespace-inferred-or-honored.md)), `<namespace>.<name>` for user vnets. Mutually exclusive with `ingressIsolationLevel`. |
| `operator.leaderElect` | bool | `true` | → `--leader-elect`. Recommended on; harmless with one replica and required for safe multi-replica HA. |
| `operator.metricsBindAddress` | string | `:8080` | → `--metrics-bind-address`. The container port is taken from it; `"0"` disables metrics, renders no metrics port, and makes `metricsService.enabled` or `podMonitor.enabled` fail the render. |
| `operator.healthProbeBindAddress` | string | `:8081` | → `--health-probe-bind-address`. |

### `webhook.*` (pod-resolution admission webhooks — ADR 0034)

Off by default. When enabled, pods are stamped with their `kube-vnet.system/net.*` membership labels during admission, closing the window in which a newly created pod is denied because the controller has not stamped it yet. The chart then installs a `MutatingWebhookConfiguration` (`failurePolicy: Ignore`) and a `ValidatingWebhookConfiguration` (`failurePolicy: Fail`), both named `<release>-pod-resolution`, a `<release>-webhook` Service, and the serving certificate. For the pods it sees, the validating webhook takes over from the system-labels `ValidatingAdmissionPolicy`; the policy keeps checking the pods the webhooks skip (the excluded namespaces below, and pods labelled `app.kubernetes.io/name=<chart name>`).

**Trade-off:** because the validating half fails closed, an operator outage blocks pod creation and update in managed namespaces. `kube-system`, `kube-public`, `kube-node-lease`, the release namespace and every namespace in `operator.disabledNamespaces` are excluded from both webhooks. Run at least two replicas. See [ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `webhook.enabled` | bool | `false` | → `--webhook-enabled` (and `--service-account-name`). |
| `webhook.certSource` | string | `helm` | `helm`: self-signed CA generated at install and reused across upgrades via `lookup`. `cert-manager`: renders a `Certificate` against `webhook.certManager.issuerRef` and relies on the CA injector for the `caBundle`. Any other value fails the render. |
| `webhook.certManager.issuerRef` | object | `{name: "", kind: Issuer, group: cert-manager.io}` | Issuer for `certSource: cert-manager`. `name` is required in that mode. |
| `webhook.timeoutSeconds` | int | `5` | Admission timeout for both webhooks. |
| `webhook.networkWait.enabled` | bool | `false` | Ship a beacon DaemonSet, headless Service and NetworkPolicy (`<release>-network-beacon`), and let pods opt into the network wait with `kube-vnet/network-max-wait` ([ADR 0045](../adr/0045-network-wait-for-opted-in-pods.md)). → `--network-wait-beacons`, `--network-wait-image`. Requires `webhook.enabled`; the render fails otherwise. |

### `dnsCarveout.*` (CoreDNS ingress carve-out — ADR 0042)

A chart-shipped `NetworkPolicy`, `kube-vnet-coredns-allow`, that keeps CoreDNS reachable on `:53` from `0.0.0.0/0` when its namespace is managed. Why and when: [recipes § managing kube-system](../guides/recipes.md#managing-kube-system-and-keeping-dns-alive).

| Key | Type | Default | Description |
|---|---|---|---|
| `dnsCarveout.enabled` | bool / null | `null` | `null` = auto (render iff `dnsCarveout.namespace` is *not* in `operator.disabledNamespaces`). `false` = never render. `true` = always render, even while the namespace is disabled. |
| `dnsCarveout.namespace` | string | `kube-system` | Namespace CoreDNS runs in; also the namespace whose managed/disabled state gates auto-rendering. |
| `dnsCarveout.selector` | map | `{k8s-app: kube-dns}` | Pod label selector for the DNS pods (the canonical label set by both kube-dns and CoreDNS). |
| `dnsCarveout.ports` | list | `[{UDP,53},{TCP,53}]` | Ingress ports opened from `0.0.0.0/0`. Add `{protocol: TCP, port: 9153}` to also expose CoreDNS metrics. |

The rendered policy carries only chart labels (`app.kubernetes.io/*`), never `kube-vnet.system/*`, so the operator's sweeps never touch it.

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

See [`security.md`](../security/security.md#who-can-write-what) for the trust-model rationale.

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
| `resources` | object | `{ limits: { cpu: 500m, memory: 256Mi }, requests: { cpu: 50m, memory: 64Mi } }` | Standard CPU/memory requests and limits. See [`operations.md`](../guides/operations.md#resource-sizing) for sizing guidance. |
| `livenessProbe.initialDelaySeconds` | int | `15` | Standard. |
| `livenessProbe.periodSeconds` | int | `20` | Standard. |
| `readinessProbe.initialDelaySeconds` | int | `5` | Standard. |
| `readinessProbe.periodSeconds` | int | `10` | Standard. |

### Optional metrics surface

| Key | Type | Default | Description |
|---|---|---|---|
| `metricsService.enabled` | bool | `false` | Create a Service (`metricsService.type`, `metricsService.port`) in front of the operator's metrics port. Useful if Prometheus scrapes via Service rather than Pod. |
| `metricsService.port` | int | `8080` | Service port. |
| `metricsService.type` | string | `ClusterIP` | Service type. |
| `podMonitor.enabled` | bool | `false` | Create a `monitoring.coreos.com/v1 PodMonitor` (requires the Prometheus operator). |
| `podMonitor.interval` | string | `30s` | Scrape interval. |
| `podMonitor.scrapeTimeout` | string | `10s` | Scrape timeout. |
| `podMonitor.labels` | object | `{}` | Extra labels on the PodMonitor (used for Prometheus-operator selector matching). |

### `cleanup.*` (uninstall hook)

A Helm **pre-delete hook** Job that removes every operator-managed `NetworkPolicy` (selector `kube-vnet.system/managed-by=kube-vnet`) cluster-wide before Helm removes the release — without it, the deny-all baselines would keep enforcing after uninstall with nothing left to manage them. It deletes the operator Deployment and waits for its pods to exit first, so drift correction cannot recreate the policies. With `webhook.enabled=true` it deletes the two `<release>-pod-resolution` webhook configurations before that, so pod admission in managed namespaces is not blocked while the operator stops. CRDs and CRs (annotated `helm.sh/resource-policy: keep`) survive uninstall. See [ADR 0036](../adr/0036-helm-pre-delete-hook-cleanup.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `cleanup.enabled` | bool | `true` | Run the pre-delete cleanup hook on `helm uninstall`. Skip ad-hoc with `helm uninstall --no-hooks` (then clean up manually). |
| `cleanup.image.repository` | string | `registry.k8s.io/kubectl` | Image for the hook Job (distroless kubectl). |
| `cleanup.image.tag` | string | `v1.30.0` | |
| `cleanup.image.pullPolicy` | string | `IfNotPresent` | |

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

kube-vnet is a control-plane operator, not a data-plane one. Existing `NetworkPolicy` keeps working while the operator is down; only change-propagation pauses. A single replica is enough for typical clusters; scaling to 2 is for node-failure resilience, not throughput. The exception is `webhook.enabled=true`: pod admission then depends on the operator, so run at least 2. See [`operations.md`](../guides/operations.md#deployment-topology).

### Why `--leader-elect=true` in the chart but `false` in the binary?

In the chart, so scaling is a one-line change. In the binary, so `make run` doesn't need leader-election RBAC.

### Why does `disabledNamespaces` default to `kube-system`?

`kube-system` holds cluster-critical pods (CoreDNS, the metrics server, the apiserver aggregator) where a deny-all baseline actually bites, so the operator stays out of it entirely by default (no baseline, no system vnets, no resolution stamping). `kube-public` and `kube-node-lease` hold no pods, so managing them is inert — they are *not* disabled by default (ADR 0042 narrowed the set from all three to just `kube-system`).

To enroll `kube-system`, remove it from the list; the chart then renders the CoreDNS carve-out. See [recipes § managing kube-system](../guides/recipes.md#managing-kube-system-and-keeping-dns-alive) and [ADR 0042](../adr/0042-coredns-ingress-carveout-and-kube-system-enrollment.md).

The operator's own namespace is implicitly added to `disabledNamespaces` so that a misconfigured `allowedNamespaces.all: true` vnet can't accidentally lock the operator out of itself.

### Why such small `resources.requests`?

Idle operator footprint is tiny — a few MB resident, near-zero CPU. The requests are sized so the operator schedules anywhere; the limits give it headroom for a reconcile burst. See [`operations.md` § Resource sizing](../guides/operations.md#resource-sizing) for when to bump.
