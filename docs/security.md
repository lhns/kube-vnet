# Security

Threat model, RBAC inventory, supply-chain practice, hardening notes, and an honest list of what kube-vnet does *not* defend against.

For the durability/AdminNetworkPolicy story specifically, see [ADR 0019](adr/0019-baseline-durability.md).

---

## Threat model

### Scope: kube-vnet only restricts ingress

The kube-vnet baseline carries `policyTypes: [Ingress]` only. Egress is unrestricted by kube-vnet (membership policies grant egress allows to vnet peers, but generic egress — DNS, the apiserver, the public internet, other namespaces — is not blocked by the operator). This is deliberate — see [ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md) — but it means you must size the threat model accordingly.

### What kube-vnet defends against

- **Accidentally-too-open ingress.** The default-allow Kubernetes posture for ingress is the bug; kube-vnet flips it to membership-based ingress allow with an ingress-deny baseline (when `ingress-isolation` is `namespace` or `pod`).
- **Drift on operator-managed `NetworkPolicy` resources** (deletion, hand-edit). The watch + reconcile loop restores the desired state within seconds and emits a `PolicyRestored` Event for visibility.
- **Misconfiguration via wrong namespace.** Pods that try to join a vnet from a non-permitted namespace appear as `InvalidJoiners` on the vnet's `Degraded` condition rather than silently failing.
- **Cross-namespace surprise.** `allowedNamespaces` is explicit; foreign namespaces don't get to join unless the vnet says so.

### What kube-vnet does NOT defend against

Be clear-eyed about these. None of them are bugs in kube-vnet; they're either out of scope or limitations of stock `NetworkPolicy`.

- **Egress exfiltration / lateral probing from a compromised pod.** kube-vnet's deny-all baseline restricts *ingress* only ([ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md)); pods can still initiate outbound traffic to anywhere they can route — other namespaces, the cluster control plane, the public internet. The vnet abstraction defends against unauthorized inbound; protecting against outbound exfiltration / lateral probing is a separate concern that needs separate tooling (a per-workload user-managed `NetworkPolicy` with `policyTypes: [Egress]`, a CNI egress firewall like Calico GlobalNetworkPolicy or Cilium FQDN policy, a NAT-gateway egress allowlist, or a service-mesh egress proxy).
- **Cluster admin compromise.** Anyone with cluster-admin (or with permissions to delete VirtualNetworks, edit the operator's RBAC, or stop the operator Deployment) can defeat kube-vnet entirely.
- **Namespace owner deleting the deny baseline.** A user with `delete networkpolicy` RBAC in their namespace can `kubectl delete networkpolicy kube-vnet`. The operator restores it within seconds (drift correction; see [`architecture.md`](architecture.md#drift-correction-loop)) and emits a `PolicyRestored` Warning Event, but during the window between deletion and restore, traffic that the policy would have denied is allowed. For a hard guarantee, the proper Kubernetes tool is `AdminNetworkPolicy` — see [ADR 0019](adr/0019-baseline-durability.md).
- **CNI bypass.** kube-vnet generates `NetworkPolicy` resources; the CNI is what enforces them. If your CNI doesn't enforce `NetworkPolicy` (or if a pod manages to bypass the CNI — e.g. host-network pods), kube-vnet's policies have no effect.
- **Layer 7 / DNS / mTLS-identity policy.** kube-vnet emits L3/L4 `NetworkPolicy`. Anything HTTP-method-aware, hostname-aware, or identity-aware is out of scope; that's a service-mesh or CNI-extension responsibility.
- **In-pod traffic.** Containers within a single pod share a network namespace and are not policy-able by Kubernetes.
- **Kernel-level escapes.** A container that escapes the kernel's network namespace boundary is a kernel CVE, not a kube-vnet concern.

### Recommended companion: per-workload egress allowlists

For workloads where outbound restriction matters, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` selecting the workload's pods and listing the destinations they're permitted to reach. NetworkPolicies compose additively; nothing in kube-vnet conflicts with this. See the [per-workload egress allowlist recipe](recipes.md#per-workload-egress-allowlist-via-user-managed-networkpolicy).

---

## RBAC inventory

The operator runs as `ServiceAccount/kube-vnet-controller` in the operator's namespace. Two role bindings:

### Cluster-scoped: `ClusterRole/kube-vnet-manager` + `ClusterRoleBinding`

| API group | Resource | Verbs | Why it's needed |
|---|---|---|---|
| `kube-vnet.lhns.de` | `virtualnetworks` | get, list, watch | Primary watch — the operator reconciles VirtualNetworks. |
| `kube-vnet.lhns.de` | `virtualnetworks/status` | get, update, patch | Writes Ready / Degraded conditions, member list, generated-policy refs. |
| `kube-vnet.lhns.de` | `virtualnetworks/finalizers` | update | Standard kubebuilder pattern; not currently used at runtime but kept for forward compatibility. |
| `networking.k8s.io` | `networkpolicies` | get, list, watch, create, update, patch, delete | The operator's primary output. Cluster-wide because `Cluster`-extent vnets generate policies in foreign namespaces. |
| `""` (core) | `pods` | get, list, watch | Membership discovery: which pods carry a join label. |
| `""` (core) | `namespaces` | get, list, watch | Honoring `kube-vnet/disabled` annotation and `allowedNamespaces.selector` matching. |
| `""` (core) | `pods` | patch | Resolution controller stamps `kube-vnet.system/net.<vnet>=<dir>` labels on managed pods (ADR 0030). |
| `kube-vnet.lhns.de` | `virtualnetworkbindings` | get, list, watch | Watch for the `VirtualNetworkBinding` reconciler; bindings produce additional membership policies. |
| `kube-vnet.lhns.de` | `virtualnetworkbindings/status` | get, update, patch | Writes the binding's `Ready` condition, `attachedPods`, `observedGeneration`. |
| `""` (core) | `events` | create, patch | Emitting Events on condition transitions and on errors (Recorder). |

### Namespace-scoped: `Role/kube-vnet-leader-election` + `RoleBinding` (in the operator's namespace only)

| API group | Resource | Verbs | Why |
|---|---|---|---|
| `coordination.k8s.io` | `leases` | get, list, watch, create, update, patch, delete | Leader election. The lease object is `kube-vnet.lhns.de` in the operator's namespace. |
| `""` (core) | `events` | create, patch | controller-runtime emits leader-election events on the lease. |

There are **no** cluster-wide write permissions on `events` (the cluster-wide ClusterRole only grants events create/patch in the namespaces where it runs operations — k8s scopes Event RBAC by the involved-object's namespace).

### What this means for blast radius

The operator can:

- **Read** every Pod, every Namespace, every VirtualNetwork in the cluster.
- **Create / modify / delete** any `NetworkPolicy` in any namespace.
- **Update** the leader-election lease in its own namespace.
- **Emit Events** on the resources it operates on.

It **cannot**:

- Read or write Secrets, ConfigMaps, ServiceAccounts, Roles, etc.
- Create / modify Pods, Namespaces, or any other resource.
- Read or write `VirtualNetwork.spec` (only `.status` is writable).

If the operator is compromised, the worst-case impact is: it can rewrite or delete every `NetworkPolicy` in the cluster, and read every Pod/Namespace label. It cannot exfiltrate secrets or pivot to other resources.

### Who can write what

The chart ships ClusterRoles aggregated into the upstream `admin`/`edit`/`view` defaults so the right authority tier writes the right CRD. With `rbac.aggregate=true` (default):

| Kind | Default writer | Mechanism |
|---|---|---|
| `VirtualNetwork` | namespace-admin | `<release>-virtualnetworks-editor` ClusterRole, aggregated into `admin`+`edit` |
| `VirtualNetworkBinding` | namespace-admin | `<release>-virtualnetworkbindings-editor`, aggregated into `admin`+`edit` |
| `VirtualNetworkBaseline` | namespace-admin | `<release>-virtualnetworkbaselines-editor`, aggregated into `admin`+`edit` |
| `ClusterVirtualNetworkBaseline` | cluster-admin | no aggregation; `<release>-clustervirtualnetworkbaselines-editor` is shipped **unbound** so cluster-admins can delegate via their own `ClusterRoleBinding` |

A matching `*-viewer` ClusterRole per CRD aggregates into `view` (or, for the cluster baseline, ships unbound).

The cluster baseline is a high-leverage knob — editing it changes every namespace's default ingress posture. Bind the editor role only to identities that already need cluster-wide policy authority. The reserved-name VAP from ADR 0031 prevents namespace-admins from sneaking around this by creating a `VirtualNetwork` named `cluster` or carrying `kube-vnet.system/managed-by=kube-vnet`.

To skip the chart-shipped end-user RBAC entirely (managing it externally via Argo, GitOps, etc.), set `rbac.aggregate=false`.

---

## Supply chain

### Container image signing (cosign keyless)

Every released image is signed with [Cosign](https://docs.sigstore.dev/cosign/) keyless, using the GitHub OIDC token as the signing identity. No long-lived keys to rotate.

The signing identity is the release workflow itself: `https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@refs/tags/<tag>`.

Verification:

```bash
cosign verify ghcr.io/lhns/kube-vnet:v0.1.0 \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

A successful verify proves the image was built and pushed by the release workflow on this repository, by a GitHub-authenticated commit. If the verify fails, do not deploy.

### Helm chart signing

The chart is also signed and published as an OCI artifact. Same verification pattern:

```bash
cosign verify ghcr.io/lhns/charts/kube-vnet:0.1.0 \
  --certificate-identity-regexp '^https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

### SBOMs

Every release ships SPDX-JSON SBOMs for both the image and the chart. They're attached as Cosign attestations *and* as plain GitHub release assets. See [`install.md`](install.md#verifying-sboms).

You can also generate one yourself:

```bash
syft ghcr.io/lhns/kube-vnet:v0.1.0 -o spdx-json
```

### Vulnerability scanning

CI runs Trivy on every PR:

- **`trivy-fs`** scans the source tree and Go module dependencies.
- **`docker`** job's image-scan step scans the locally-built image.

Both jobs fail the build on `CRITICAL` or `HIGH` severity findings (`ignore-unfixed: true` — issues without an upstream fix don't gate the build).

For your own deployments, run Trivy or Grype against the deployed image periodically:

```bash
trivy image ghcr.io/lhns/kube-vnet:v0.1.0 --severity CRITICAL,HIGH
```

### Dependency updates

Dependabot is configured (`.github/dependabot.yml`) for:

- **`gomod`** — Go modules. `k8s.io/*` and `sigs.k8s.io/*` are grouped (controller-runtime pins specific k8s.io versions; they need to move together). Other Go deps are grouped under `go-deps`.
- **`github-actions`** — workflow `uses:` refs.
- **`docker`** — the Dockerfile `FROM` image.

Schedule: weekly Mondays. PRs are labeled per ecosystem with caps so the queue doesn't flood.

---

## Hardening

### Container

- **Image**: `gcr.io/distroless/static:nonroot`. Statically-linked Go binary; no shell, no package manager, no setuid binaries.
- **User**: `65532:65532` (the `nonroot` user from distroless).
- **Read-only root filesystem**: yes.
- **All capabilities dropped**: `securityContext.capabilities.drop: [ALL]`.
- **No privilege escalation**: `allowPrivilegeEscalation: false`.
- **seccomp**: `RuntimeDefault`.

These are configured both in `config/manager/manager.yaml` (the Kustomize install) and the Helm chart's `values.yaml` defaults. Override in Helm values if your environment requires a different profile.

### Network

- The operator container exposes two ports:
  - `:8080` — Prometheus metrics. Not exposed via a Service by default; opt in with `metricsService.enabled=true` or `podMonitor.enabled=true`.
  - `:8081` — health/readiness probes. Cluster-internal.
- It makes egress only to the apiserver (and to CoreDNS for resolution).
- It does not need any inbound connectivity from outside the cluster.

### Identity / authentication

- The operator authenticates to the apiserver via its `ServiceAccount` token (the standard projected-volume mechanism).
- Leader election uses the same identity to update the lease.
- No external secrets, no service mesh dependency.

---

## Common security questions

### Can the operator be locked down further by removing some permissions?

Not without losing functionality. Each line in the RBAC inventory above maps to a feature. The most-asked-about removal is `networkpolicies` cluster-wide write — but that's required for `Cluster`-extent vnets to install policies in foreign namespaces, and trimming it to namespace-scoped would require the operator to be installed per-namespace, which defeats the design.

### Can a namespace owner permanently disable kube-vnet for their namespace?

Yes — by annotating the namespace `kube-vnet/disabled: "true"`. This removes the operator's baseline and any membership policies; pods in the namespace are not eligible joiners for any vnet. See [ADR 0006](adr/0006-baseline-default-deny-and-single-opt-out.md).

If you want to prevent namespace owners from doing this, withhold `update namespace` (or specifically `patch namespace`) RBAC from them. Standard Kubernetes RBAC; nothing kube-vnet-specific.

### Is kube-vnet a good fit for multi-tenant clusters?

Conditionally yes. kube-vnet enforces tenant isolation **at the NetworkPolicy layer**, which is good but not a hard tenant boundary. Strict multi-tenancy needs more (admission control, RBAC partitioning, quota, possibly virtual clusters). Treat kube-vnet as one layer of defense in a broader multi-tenancy strategy.

The deny-baseline durability concern (next section) is especially relevant in multi-tenant clusters because tenants typically have NetworkPolicy CRUD in their own namespaces.

### Does the operator log secrets?

No. The operator never reads Secrets, ConfigMaps with sensitive data, or pod environment variables. Logs include resource names, namespaces, and counts, but never spec content beyond what kube-vnet itself wrote.

---

## The AdminNetworkPolicy future

Stock `NetworkPolicy` is namespace-local. A namespace owner with `delete networkpolicy` RBAC can remove kube-vnet's deny baseline; the operator restores it within seconds, but the window exists.

The proper Kubernetes-native answer is `policy.networking.k8s.io/v1 AdminNetworkPolicy` (ANP):

- **Cluster-scoped resource** — namespace-level RBAC has no authority over it.
- **Distinct API group** — ANP RBAC is granted separately from `NetworkPolicy` RBAC, so cluster admins can grant NP wide while keeping ANP locked down.
- **Higher precedence** — an ANP `Deny` overrides any matching NP `Allow`. The deny baseline becomes a hard guarantee, not a reconciliation race.

Adoption is deferred for now (CNI support is still maturing across the ecosystem; the API itself is `v1alpha1`/`v1beta1` depending on version). The current drift-correction defense plus `PolicyRestored` events is sufficient for the dominant threat (accidental deletion or unaware tooling). When ANP support is broad enough, the deny baseline migrates to a single cluster-scoped ANP; per-vnet allow policies stay as `NetworkPolicy`.

Full discussion: [ADR 0019](adr/0019-baseline-durability.md).

---

## Reporting a vulnerability

The repo lives at https://github.com/lhns/kube-vnet. Use GitHub's private security advisory mechanism (the **Security** tab → **Report a vulnerability**) if you find a credible vulnerability. For public, low-severity issues, a normal GitHub Issue is fine.
