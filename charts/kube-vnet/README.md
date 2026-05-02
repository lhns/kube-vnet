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
| `operator.labelPrefix` | `kube-vnet/` | Label-key prefix for join labels |
| `operator.disabledNamespaces` | `[]` | Namespaces the operator never touches (mirrors `kube-vnet/disabled=true`) |
| `operator.ingressIsolation.mode` | **(required)** | Cluster-wide ingress-isolation mode (`none`/`namespace`/`pod`) |
| `operator.ingressIsolation.namespaceOverrides.none` | `[kube-system, kube-public, kube-node-lease]` | Namespaces overridden to `none` (control-plane safety) |
| `operator.leaderElect` | `true` | Enable leader election |
| `metricsService.enabled` | `false` | Expose `/metrics` via a Service |
| `podMonitor.enabled` | `false` | Create a `PodMonitor` for the Prometheus operator |
| `resources.*` | small defaults | CPU/memory requests and limits |
| `nodeSelector` / `tolerations` / `affinity` | empty | Standard Pod scheduling overrides |

See [`values.yaml`](./values.yaml) for the full set.

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
