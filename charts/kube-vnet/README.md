# kube-vnet Helm chart

Installs the [kube-vnet](https://github.com/lhns/kube-vnet) operator —
a Kubernetes operator that translates `VirtualNetwork` membership into
standard `NetworkPolicy` resources.

## Install

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --namespace kube-vnet-system \
  --create-namespace
```

To install a specific image tag (e.g. for a pre-release):

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --set image.tag=v0.1.0-rc.1 \
  --namespace kube-vnet-system --create-namespace
```

## Verify the chart and image (cosign keyless)

```bash
cosign verify ghcr.io/lhns/kube-vnet:v0.1.0 \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'

cosign verify ghcr.io/lhns/charts/kube-vnet:0.1.0 \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/lhns/kube-vnet` | Operator image repository |
| `image.tag` | `""` (chart appVersion) | Operator image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `replicaCount` | `1` | Operator replicas (scale to 2+ for HA; leader election always on) |
| `operator.disabledNamespaces` | `[kube-system, kube-public, kube-node-lease]` | Namespaces the operator never touches (mirrors `kube-vnet/disabled=true`) |
| `operator.clusterBaseline.create` | `true` | Whether the chart seeds the singleton `ClusterVirtualNetworkBaseline` named `default` |
| `operator.clusterBaseline.ingressIsolationLevel` | `""` (REQUIRED if `create=true` and `memberships` unset) | Preset: `pod` \| `namespace` \| `cluster`. See ADR 0031. |
| `operator.clusterBaseline.memberships` | `null` | Explicit override map: `<vnet-key>: <direction>`. Mutually exclusive with `ingressIsolationLevel`. |
| `operator.leaderElect` | `true` | Enable leader election |
| `rbac.aggregate` | `true` | Ship aggregated end-user ClusterRoles for the namespace-scoped CRDs (auto-merge into upstream `admin`/`edit`/`view`) plus an unbound editor + viewer pair for `ClusterVirtualNetworkBaseline`. Set `false` to manage all RBAC outside Helm. |
| `cleanup.enabled` | `true` | Run a pre-delete hook that removes operator-managed NetworkPolicies on `helm uninstall`. Without it the deny-all baselines survive uninstall and keep enforcing. See ADR 0036. |
| `cleanup.image.repository` | `registry.k8s.io/kubectl` | Image for the pre-delete hook Job. Published by the Kubernetes SIG Release team — no single-vendor dependency. |
| `cleanup.image.tag` | `"v1.30.0"` | Tag for the pre-delete hook image. Pin to a known-good kubectl version (uses `v`-prefixed semver per `registry.k8s.io/kubectl`). |
| `metricsService.enabled` | `false` | Expose `/metrics` via a Service |
| `podMonitor.enabled` | `false` | Create a `PodMonitor` for the Prometheus operator |
| `resources.*` | small defaults | CPU/memory requests and limits |
| `nodeSelector` / `tolerations` / `affinity` | empty | Standard Pod scheduling overrides |

See [`values.yaml`](./values.yaml) for the full set.

## End-user RBAC

By default (`rbac.aggregate: true`) the chart ships ClusterRoles aggregated into the upstream `admin`, `edit`, and `view` ClusterRoles for `VirtualNetwork`, `VirtualNetworkBinding`, and `VirtualNetworkBaseline`. Anyone bound to one of those upstream roles within a namespace automatically gains the corresponding access on the kube-vnet CRDs in that namespace — no extra bindings to create.

`ClusterVirtualNetworkBaseline` (cluster-scoped) is **not** aggregated; only cluster-admin can write it by default. The chart ships an unbound `<release>-clustervirtualnetworkbaselines-editor` ClusterRole for cluster-admins to bind explicitly via their own `ClusterRoleBinding` if they want to delegate cluster-baseline editing to a platform-team user/group. **Bind carefully**: the cluster baseline drives every namespace's default ingress posture.

A matching viewer ClusterRole (`<release>-clustervirtualnetworkbaselines-viewer`) lets dashboards and audit tooling read the cluster baseline without write access.

## Defining a VirtualNetwork

Once the chart is installed, the `VirtualNetwork` CRD is registered. See the
project README for usage and runnable examples:

- <https://github.com/lhns/kube-vnet#how-it-works-a-worked-example>
- <https://github.com/lhns/kube-vnet/tree/main/config/samples>

## Uninstall

```bash
helm uninstall kube-vnet --namespace kube-vnet-system
```

The CRD is **not** removed by `helm uninstall` (Helm intentionally preserves
CRDs to avoid taking down dependent resources). To remove it:

```bash
kubectl delete crd virtualnetworks.kube-vnet.lhns.de
```
