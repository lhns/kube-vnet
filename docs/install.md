# Install

Three install paths, in order of preference: **Helm** (recommended), **`kubectl apply` of `release.yaml`**, or **`kubectl apply -k config/default`** from the source tree. All three install the same resources — the difference is how you parameterize and upgrade.

---

## Prerequisites

### Kubernetes version

`>= 1.25`. The CRD uses an `x-kubernetes-validations` (CEL) rule for name validation; CEL became Generally Available in 1.25.

### CNI that enforces NetworkPolicy

kube-vnet *generates* `networking.k8s.io/v1` `NetworkPolicy` resources. Your CNI is what actually drops packets. Without a `NetworkPolicy`-enforcing CNI, kube-vnet's policies have no effect.

Known compatible CNIs:

- **Calico** (any reasonably recent version)
- **Cilium**
- **kube-router** (the standalone NP controller)
- **Antrea**
- **kindnetd** in recent versions of `kind` (NetworkPolicy support is newer; verify against your kind version)

Hosted Kubernetes:

- **GKE** — Dataplane V2 (or "Network Policy" add-on enabled).
- **EKS** — Calico, Cilium, or VPC CNI with `enableNetworkPolicy=true`.
- **AKS** — Azure CNI Powered by Cilium, Calico, or Azure NPM.

If you're not sure whether your cluster enforces NetworkPolicy, the [e2e tests](development.md) in this repo will tell you within minutes.

### Permissions

You need cluster-admin (or equivalent) for the install — the operator needs cluster-wide read on Pods and Namespaces and cluster-wide CRUD on `networkpolicies.networking.k8s.io`. See [`security.md`](security.md) for the full RBAC inventory and rationale.

---

## Helm install (recommended)

The chart is published as an OCI artifact to `ghcr.io/lhns/charts/kube-vnet`. Helm 3.8+ supports OCI registries natively; no separate repository to add.

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --namespace kube-vnet-system --create-namespace
```

Replace `0.1.0` with the version you want — see the [GitHub releases page](https://github.com/lhns/kube-vnet/releases) for tags.

### Common values

```bash
# Pin a specific image tag (default: chart appVersion)
helm install ... --set image.tag=v0.1.0

# Cluster-wide ingress-isolation default (none|namespace|pod). See concepts.md and ADRs 0023-0025.
helm install ... --set operator.ingressIsolation.mode=pod

# Per-mode override lists carve out exceptions to the cluster-wide default.
helm install ... \
  --set 'operator.ingressIsolation.mode=pod' \
  --set 'operator.ingressIsolation.forceNone={legacy,sandbox}' \
  --set 'operator.ingressIsolation.forceNamespace={team-a,team-b}'

# Customize the operator-level exclusion list (the operator's own ns is auto-added)
helm install ... --set 'operator.excludedNamespaces={kube-system,kube-public,kube-node-lease,my-legacy-ns}'

# Expose the metrics endpoint via a Service (off by default)
helm install ... --set metricsService.enabled=true

# Create a Prometheus PodMonitor (requires the Prometheus Operator)
helm install ... --set podMonitor.enabled=true
```

The full value reference is in [`reference/configuration.md`](reference/configuration.md). The chart's own README is [`charts/kube-vnet/README.md`](../charts/kube-vnet/README.md).

### Upgrading

```bash
helm upgrade kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version <new> \
  --namespace kube-vnet-system \
  --reuse-values
```

`--reuse-values` keeps the values from the previous install. Drop it (and pass `--values yourfile.yaml`) when you want to change values explicitly.

CRD upgrades: Helm intentionally does **not** upgrade CRDs on `helm upgrade` (Helm's CRD policy). If a release ships a CRD change, apply it explicitly:

```bash
kubectl apply -f https://github.com/lhns/kube-vnet/releases/download/<tag>/release.yaml \
  --selector apiextensions.k8s.io/v1=CustomResourceDefinition
```

Or apply the chart's CRD directly:

```bash
helm pull oci://ghcr.io/lhns/charts/kube-vnet --version <tag> --untar
kubectl apply -f kube-vnet/crds/
```

### Uninstalling

```bash
helm uninstall kube-vnet --namespace kube-vnet-system
```

The CRD is **not** removed by `helm uninstall` — Helm preserves CRDs to avoid taking down dependent resources. To remove it:

```bash
kubectl delete crd virtualnetworks.kube-vnet.lhns.de
```

This will cascade-delete every `VirtualNetwork` resource and (because the per-vnet membership policies have owner references in the home namespace) the operator-managed `NetworkPolicy` resources too. Cross-namespace policies (foreign to the home) need to be cleaned up manually if the operator was already gone — the operator normally handles them via its `kube-vnet/network` label, but it can't if it's already uninstalled. Cleanup pattern:

```bash
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet -o name \
  | xargs -I{} kubectl delete -A {}
```

---

## `kubectl apply` install

Each release has a `release.yaml` asset that is the rendered output of `kubectl kustomize config/default`. One file, no Helm:

```bash
kubectl apply -f https://github.com/lhns/kube-vnet/releases/download/v0.1.0/release.yaml
```

This installs:

- The `kube-vnet-system` namespace.
- The `VirtualNetwork` CRD.
- The `kube-vnet-controller` ServiceAccount + ClusterRole + ClusterRoleBinding.
- The leader-election Role + RoleBinding in `kube-vnet-system`.
- The `kube-vnet-controller` Deployment.

To configure the operator (flags, replicas), you'd edit the rendered manifest before applying or use the Helm install — `release.yaml` is for the simplest case.

Uninstall is symmetric:

```bash
kubectl delete -f https://github.com/lhns/kube-vnet/releases/download/v0.1.0/release.yaml
```

---

## From-source install

If you've cloned the repository:

```bash
kubectl apply -k config/default
```

This is equivalent to `release.yaml` but always tracks `main`. Useful for testing changes before they're tagged.

---

## Verifying signatures

Every released image and Helm chart is signed with [Cosign](https://docs.sigstore.dev/cosign/) keyless via the GitHub OIDC token (no long-lived keys). The signing identity is the release workflow itself.

Verify the container image:

```bash
cosign verify ghcr.io/lhns/kube-vnet:v0.1.0 \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

Verify the Helm chart artifact:

```bash
cosign verify ghcr.io/lhns/charts/kube-vnet:0.1.0 \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

A successful verification prints the matching certificate, the issuer, and the digest. Any other output (or a non-zero exit code) means the signature didn't validate against the workflow identity.

## Verifying SBOMs

Each release ships SPDX-JSON SBOMs for both the image and the chart. They're attached as Cosign attestations *and* uploaded as plain release assets.

Pull and verify the image SBOM attestation:

```bash
cosign verify-attestation ghcr.io/lhns/kube-vnet:v0.1.0 \
  --type spdx \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  | jq -r '.payload | @base64d | fromjson | .predicate' \
  > image.sbom.spdx.json
```

Or just download the asset:

```bash
curl -sLo image.sbom.spdx.json \
  https://github.com/lhns/kube-vnet/releases/download/v0.1.0/kube-vnet-image.sbom.spdx.json
```

`checksums.txt` in each release covers all assets with SHA-256 sums.

---

## Air-gapped installs

The operator binary needs:

1. The container image (`ghcr.io/lhns/kube-vnet:<tag>`) — pull, retag, and push to your internal registry.
2. The CRD and manifests (`release.yaml`) — copy and apply.

The runtime image is `gcr.io/distroless/static:nonroot` (the binary is statically linked Go). No external runtime dependencies; once the image is in your registry, the operator runs offline.

If you use Helm, mirror `oci://ghcr.io/lhns/charts/kube-vnet:<chart-version>` to your internal OCI registry too:

```bash
helm pull oci://ghcr.io/lhns/charts/kube-vnet --version 0.1.0
helm push kube-vnet-0.1.0.tgz oci://internal.example/charts
```

---

## Sanity-check after install

```bash
# Operator running
kubectl get deploy -n kube-vnet-system kube-vnet-controller

# CRD registered
kubectl get crd virtualnetworks.kube-vnet.lhns.de

# Apply a sample
kubectl apply -f https://raw.githubusercontent.com/lhns/kube-vnet/main/config/samples/01_same_namespace.yaml

# See what the operator generated
kubectl get vnet -A
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet
```

If the operator isn't producing policies, see [`troubleshooting.md`](troubleshooting.md).
