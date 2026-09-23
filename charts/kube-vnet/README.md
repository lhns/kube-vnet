# kube-vnet Helm chart

Installs the [kube-vnet](https://github.com/lhns/kube-vnet) operator —
a Kubernetes operator that translates `VirtualNetwork` membership into
standard `NetworkPolicy` resources.

## Install

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --namespace kube-vnet-system --create-namespace \
  --set operator.clusterBaseline.ingressIsolationLevel=cluster   # required: pod | namespace | cluster
```

`operator.clusterBaseline.ingressIsolationLevel` (or an explicit `operator.clusterBaseline.memberships` map) is required; the chart fails without one. To install a specific image tag, add `--set image.tag=<tag>`.

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
| `webhook.enabled` | `false` | Stamp pod membership during admission (ADR 0034), closing the window in which a starting pod is denied because it is not yet stamped. The validating half is `failurePolicy: Fail`: an operator outage blocks pod creation in managed namespaces. Run 2+ replicas. |
| `webhook.certSource` | `helm` | `helm` (self-signed CA, reused across upgrades via `lookup`) or `cert-manager` |
| `webhook.certManager.issuerRef` | `{name: "", kind: Issuer, group: cert-manager.io}` | Issuer for `certSource: cert-manager`; `name` is required in that mode |
| `webhook.timeoutSeconds` | `5` | Admission timeout. Resolution is served from cache, so a slow reply means the operator is unhealthy |
| `webhook.networkWait.enabled` | `false` | Ship per-node network beacons and let pods opt in with `kube-vnet/network-max-wait: "30s"`: the app starts once every node has applied the pod's rules, or when the maximum runs out (ADR 0045). Requires `webhook.enabled` |
| `image.repository` | `ghcr.io/lhns/kube-vnet` | Operator image repository |
| `image.tag` | `""` (chart appVersion) | Operator image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `replicaCount` | `1` | Operator replicas (2+ for HA, and with `webhook.enabled`) |
| `operator.disabledNamespaces` | `[kube-system]` | Namespaces the operator never touches (mirrors `kube-vnet/disabled=true`). Removing `kube-system` enrolls it; `dnsCarveout` then keeps CoreDNS reachable |
| `dnsCarveout.enabled` | `null` (auto) | Render the NetworkPolicy that keeps CoreDNS reachable on `:53`: automatically when `dnsCarveout.namespace` is managed, or forced with `true`/`false` (ADR 0042) |
| `operator.apiserverSourceCIDR` | `0.0.0.0/0` | Source CIDR of the auto-allow for Services the apiserver dials (webhooks, APIServices) |
| `operator.clusterBaseline.create` | `true` | Whether the chart seeds the singleton `ClusterVirtualNetworkBaseline` named `default` |
| `operator.clusterBaseline.ingressIsolationLevel` | `""` (required unless `memberships` is set) | Preset: `pod` \| `namespace` \| `cluster`. See ADR 0031. |
| `operator.clusterBaseline.memberships` | `null` | Explicit override map: `<vnet-key>: <direction>`. Mutually exclusive with `ingressIsolationLevel`. |
| `operator.leaderElect` | `true` | Enable leader election |
| `rbac.aggregate` | `true` | Ship aggregated end-user ClusterRoles for the namespace-scoped CRDs (auto-merge into upstream `admin`/`edit`/`view`) plus an unbound editor + viewer pair for `ClusterVirtualNetworkBaseline`. Set `false` to manage all RBAC outside Helm. |
| `cleanup.enabled` | `true` | Run a pre-delete hook that removes operator-managed NetworkPolicies on `helm uninstall`. Without it the deny-all baselines survive uninstall and keep enforcing. See ADR 0036. |
| `cleanup.image.repository` | `registry.k8s.io/kubectl` | Image for the pre-delete hook Job |
| `cleanup.image.tag` | `"v1.30.0"` | Tag for the pre-delete hook image (`v`-prefixed, as `registry.k8s.io/kubectl` publishes them) |
| `metricsService.enabled` | `false` | Expose `/metrics` via a Service |
| `podMonitor.enabled` | `false` | Create a `PodMonitor` for the Prometheus operator |
| `resources.*` | small defaults | CPU/memory requests and limits |
| `nodeSelector` / `tolerations` / `affinity` | empty | Standard Pod scheduling overrides |

Full reference: [`docs/reference/configuration.md`](https://github.com/lhns/kube-vnet/blob/main/docs/reference/configuration.md), or [`values.yaml`](./values.yaml).

## End-user RBAC

By default (`rbac.aggregate: true`) the chart ships ClusterRoles aggregated into the upstream `admin`, `edit`, and `view` ClusterRoles for `VirtualNetwork`, `VirtualNetworkBinding`, and `VirtualNetworkBaseline`. Anyone bound to one of those upstream roles within a namespace automatically gains the corresponding access on the kube-vnet CRDs in that namespace — no extra bindings to create.

`ClusterVirtualNetworkBaseline` (cluster-scoped) is **not** aggregated; only cluster-admin can write it by default. The chart ships an unbound `<release>-clustervirtualnetworkbaselines-editor` ClusterRole for cluster-admins to bind explicitly via their own `ClusterRoleBinding` if they want to delegate cluster-baseline editing to a platform-team user/group. Bind it carefully: the cluster baseline drives every namespace's default ingress posture.

A matching viewer ClusterRole (`<release>-clustervirtualnetworkbaselines-viewer`) lets dashboards and audit tooling read the cluster baseline without write access.

## Defining a VirtualNetwork

Once the chart is installed, the `VirtualNetwork` CRD is registered. Walkthrough and runnable examples:

- <https://github.com/lhns/kube-vnet/blob/main/docs/getting-started/first-vnet.md>
- <https://github.com/lhns/kube-vnet/tree/main/config/samples>

## Uninstall

```bash
helm uninstall kube-vnet --namespace kube-vnet-system
```

A pre-delete hook first removes the operator-managed NetworkPolicies
(`cleanup.enabled`), and with `webhook.enabled` the pod-resolution webhook
configurations before that. The four CRDs and the seeded `ClusterVirtualNetworkBaseline`
carry `helm.sh/resource-policy: keep` and survive uninstall. To remove the CRDs
(and with them every kube-vnet custom resource):

```bash
kubectl delete crd virtualnetworks.kube-vnet.lhns.de virtualnetworkbindings.kube-vnet.lhns.de \
  virtualnetworkbaselines.kube-vnet.lhns.de clustervirtualnetworkbaselines.kube-vnet.lhns.de
```
