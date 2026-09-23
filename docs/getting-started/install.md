# Install

Three install paths, in order of preference: **Helm** (recommended), **`kubectl apply` of `release.yaml`**, or **`kubectl apply -k config/default`** from the source tree. Only the Helm chart seeds the cluster baseline, installs the uninstall cleanup hook, the CoreDNS carve-out, the optional admission webhook and network wait, and gates the admission policies on the Kubernetes version.

---

## Prerequisites

### Kubernetes version

`>= 1.25`. The CRD uses an `x-kubernetes-validations` (CEL) rule for name validation; CEL became Generally Available in 1.25.

On Kubernetes ≥ 1.30 the chart also installs three `ValidatingAdmissionPolicies`: one rejects `kube-vnet/net.*` label values other than `both`, `ingress`, `egress`, `none`; one protects the operator-owned `kube-vnet.system/*` labels; one reserves the system vnet names. Below 1.30 the chart skips them and prints a warning. Invalid join-label values are then still ignored at reconcile time and reported (an `InvalidJoinLabelDirection` event on the pod, `InvalidJoiners` on the vnet), but the operator-owned labels are protected only by drift correction — see [threat model F-02](../security/threat-model.md#7-findings-register).

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

If you're not sure whether your cluster enforces NetworkPolicy, the [e2e tests](../internals/development.md) in this repo will tell you within minutes.

### Permissions

You need cluster-admin (or equivalent) for the install: the operator gets a ClusterRole with cluster-wide `NetworkPolicy` CRUD and `pods` patch, among others. See [`security.md`](../security/security.md#rbac-inventory) for the full inventory.

---

## Helm install (recommended)

The chart is published as an OCI artifact to `ghcr.io/lhns/charts/kube-vnet`. Helm 3.8+ supports OCI registries natively; no separate repository to add.

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version <version> \
  --namespace kube-vnet-system --create-namespace \
  --set operator.clusterBaseline.ingressIsolationLevel=cluster   # or namespace | pod
```

`<version>` is a tag from the [releases page](https://github.com/lhns/kube-vnet/releases) without the `v` (e.g. `0.7.3`).

`operator.clusterBaseline.ingressIsolationLevel` is **required** ([ADR 0031](../adr/0031-baseline-tier-resolution.md)): the chart fails if neither it nor `operator.clusterBaseline.memberships` is set while `create=true`. What the three levels mean: [first-vnet § isolation level](first-vnet.md#2-decide-your-isolation-level--before-you-install).

The default `operator.disabledNamespaces` is `[kube-system]`; the operator stays out of it entirely (no baseline, no system vnets, no resolution stamping). Removing it enrolls `kube-system`, and the chart then renders the CoreDNS carve-out ([ADR 0042](../adr/0042-coredns-ingress-carveout-and-kube-system-enrollment.md)).

#### What the default install means for new namespaces

Every managed namespace gets the deny-all baseline. Pods opt in via the seeded `ClusterVirtualNetworkBaseline` (chart-managed; one of three presets via `ingressIsolationLevel`), per-NS `VirtualNetworkBaseline`, per-workload `VirtualNetworkBinding`, or the `kube-vnet/net.<vnet>` pod label — additive ingress allows from vnet peers; everything else is denied. To opt a namespace out entirely:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: my-app
  annotations:
    kube-vnet/disabled: "true"
```

### Common values

```bash
# Pin a specific image tag (default: chart appVersion)
helm install ... --set image.tag=v<version>

# Same-NS connectivity by default (every pod auto-joins the per-NS `namespace` system vnet
# at default-both; can egress to anything cluster-wide).
helm install ... --set operator.clusterBaseline.ingressIsolationLevel=namespace

# No isolation (allow-all) — every pod is on the `cluster` system vnet at default-both.
helm install ... --set operator.clusterBaseline.ingressIsolationLevel=cluster

# Strict pod-level isolation — ingress only via explicit binding/label.
helm install ... --set operator.clusterBaseline.ingressIsolationLevel=pod

# Customize the operator-level disabled-namespaces list (operator's own ns is auto-added)
helm install ... --set 'operator.disabledNamespaces={kube-system,my-legacy-ns}'

# Stamp pod membership at admission (ADR 0034); read the trade-off first
helm install ... --set webhook.enabled=true --set replicaCount=2

# Also let pods opt into the network wait with kube-vnet/network-max-wait (ADR 0045)
helm install ... --set webhook.enabled=true --set webhook.networkWait.enabled=true

# Expose the metrics endpoint via a Service (off by default)
helm install ... --set metricsService.enabled=true

# Create a Prometheus PodMonitor (requires the Prometheus Operator)
helm install ... --set podMonitor.enabled=true
```

The full value reference is in [`reference/configuration.md`](../reference/configuration.md). The chart's own README is [`charts/kube-vnet/README.md`](../../charts/kube-vnet/README.md).

### Upgrading

```bash
helm upgrade kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version <new> \
  --namespace kube-vnet-system \
  --reuse-values
```

`--reuse-values` keeps the values from the previous install. Drop it (and pass `--values yourfile.yaml`) when you want to change values explicitly.

The chart ships its CRDs as regular templates (not under `crds/`), so `helm upgrade` updates them too.

### Uninstalling

```bash
helm uninstall kube-vnet --namespace kube-vnet-system
```

A pre-delete hook removes every operator-managed `NetworkPolicy` first, so namespaces return to default-allow ([ADR 0036](../adr/0036-helm-pre-delete-hook-cleanup.md); disable with `cleanup.enabled=false` or `--no-hooks`). The four CRDs and the seeded `ClusterVirtualNetworkBaseline` carry `helm.sh/resource-policy: keep` and survive, so a later install resumes with your CRs. To remove them too:

```bash
kubectl delete crd virtualnetworks.kube-vnet.lhns.de virtualnetworkbindings.kube-vnet.lhns.de \
  virtualnetworkbaselines.kube-vnet.lhns.de clustervirtualnetworkbaselines.kube-vnet.lhns.de
```

If policies are left over (hook skipped or failed), delete them by label:

```bash
kubectl delete networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet
```

### Testing a dev build

Every push to any branch (and every `workflow_dispatch` run of the release workflow) publishes a development image and chart tagged `0.0.0-dev.<short-sha>`. These are signed and SBOM'd just like releases — no GitHub Release is created — so you can install a specific commit on your cluster without waiting for a tag:

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.0.0-dev.abc1234 \
  --namespace kube-vnet-system --create-namespace
```

To find the latest dev sha for a branch (requires `gh` CLI auth):

```bash
gh api repos/lhns/kube-vnet/actions/runs --jq \
  '.workflow_runs[] | select(.head_branch=="main" and .name=="release" and .conclusion=="success") | .head_sha[0:7]' \
  | head -1
```

The image is also tagged `:sha-<short>` (raw SHA alias) and `:<branch>` (a moving "latest from this branch" tag, useful for ephemeral test environments). The chart only carries the immutable `0.0.0-dev.<short-sha>` version — never overwritten.

Dev builds are single-arch (`linux/amd64`) and use the GitHub Actions buildx cache, so they typically finish in 2–4 minutes versus the ~10–15 minutes of a multi-arch tagged release.

---

## `kubectl apply` install

Each release has a `release.yaml` asset that is the rendered output of `kubectl kustomize config/default`, with the operator image pinned to the release tag. One file, no Helm:

```bash
kubectl apply -f https://github.com/lhns/kube-vnet/releases/latest/download/release.yaml
# or a specific release: .../releases/download/v<version>/release.yaml
```

This installs:

- The `kube-vnet-system` namespace.
- The four CRDs.
- The `kube-vnet-controller` ServiceAccount, `kube-vnet-manager` ClusterRole + binding, and the leader-election Role + RoleBinding.
- The `kube-vnet-controller` Deployment (flag defaults, so `--disabled-namespaces` is `kube-system`).
- The three `ValidatingAdmissionPolicies` — unconditionally, so this path needs Kubernetes ≥ 1.30.

It does **not** create a `ClusterVirtualNetworkBaseline`. Without one, pods get no default memberships — the strictest posture. Apply one yourself, e.g. [sample 09](../../config/samples/09_clustervirtualnetworkbaseline.yaml) (the `namespace` preset). It also ships no uninstall hook: `kubectl delete -f release.yaml` leaves the generated policies behind, so delete them by label first (see [Uninstalling](#uninstalling)).

To configure the operator (flags, replicas), edit the rendered manifest before applying, or use the Helm install.

---

## From-source install

If you've cloned the repository:

```bash
kubectl apply -k config/default
```

This is equivalent to `release.yaml` but always tracks `main` and runs the `ghcr.io/lhns/kube-vnet:latest` image. Useful for testing changes before they're tagged.

---

## Verifying signatures

Every released image and Helm chart is signed with [Cosign](https://docs.sigstore.dev/cosign/) keyless via the GitHub OIDC token (no long-lived keys). The signing identity is the release workflow itself.

Verify the container image:

```bash
cosign verify ghcr.io/lhns/kube-vnet:v<version> \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

Verify the Helm chart artifact:

```bash
cosign verify ghcr.io/lhns/charts/kube-vnet:<version> \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

A successful verification prints the matching certificate, the issuer, and the digest. Any other output (or a non-zero exit code) means the signature didn't validate against the workflow identity.

## Verifying SBOMs

Each release ships SPDX-JSON SBOMs for both the image and the chart. They're attached as Cosign attestations *and* uploaded as plain release assets.

Pull and verify the image SBOM attestation:

```bash
cosign verify-attestation ghcr.io/lhns/kube-vnet:v<version> \
  --type spdx \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  | jq -r '.payload | @base64d | fromjson | .predicate' \
  > image.sbom.spdx.json
```

Or just download the asset:

```bash
curl -sLo image.sbom.spdx.json \
  https://github.com/lhns/kube-vnet/releases/download/v<version>/kube-vnet-image.sbom.spdx.json
```

`checksums.txt` in each release covers all assets with SHA-256 sums.

---

## Air-gapped installs

The operator binary needs:

1. The container image (`ghcr.io/lhns/kube-vnet:<tag>`) — pull, retag, and push to your internal registry.
2. The CRD and manifests (`release.yaml`) — copy and apply.

The runtime image is `gcr.io/distroless/static:nonroot` (the binary is statically linked Go). No external runtime dependencies; once the image is in your registry, the operator runs offline.

If you use Helm, mirror `oci://ghcr.io/lhns/charts/kube-vnet:<chart-version>` to your internal OCI registry too, plus the uninstall hook's `registry.k8s.io/kubectl` image (`cleanup.image.*`):

```bash
helm pull oci://ghcr.io/lhns/charts/kube-vnet --version <version>
helm push kube-vnet-<version>.tgz oci://internal.example/charts
```

---

## Sanity-check after install

```bash
# Operator running (kustomize/release.yaml installs name it kube-vnet-controller)
kubectl get deploy -n kube-vnet-system kube-vnet

# CRD registered
kubectl get crd virtualnetworks.kube-vnet.lhns.de

# Apply a sample
kubectl apply -f https://raw.githubusercontent.com/lhns/kube-vnet/main/config/samples/01_same_namespace.yaml

# See what the operator generated
kubectl get vnet -A
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet
```

If the operator isn't producing policies, see [`troubleshooting.md`](../guides/troubleshooting.md).
