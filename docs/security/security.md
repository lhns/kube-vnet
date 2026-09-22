# Security

Threat model, RBAC inventory, supply-chain practice, hardening notes, and an honest list of what kube-vnet does *not* defend against.

> For the **formal STRIDE threat model** — assets, actors, trust boundaries, data-flow diagrams, and the findings register — see [`threat-model.md`](threat-model.md). This page is the operator-facing guide; that one is the model.

For the durability/AdminNetworkPolicy story specifically, see [ADR 0019](../adr/0019-baseline-durability.md).

---

## Threat model

### Scope: kube-vnet only restricts ingress

Every policy kube-vnet emits is `policyTypes: [Ingress]`. Egress — DNS, the apiserver, the public internet, other namespaces — is not restricted. This is deliberate ([ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md)); size your threat model accordingly.

### What kube-vnet defends against

- **Accidentally-too-open ingress.** The default-allow Kubernetes posture for ingress is the bug; kube-vnet flips it to membership-based ingress allow with the uniform ingress-deny baseline `kube-vnet.base` (how much of it bites is set by the [baseline tier](../getting-started/concepts.md#the-deny-all-baseline) — the `pod`/`namespace`/`cluster` presets and their per-namespace overrides).
- **Drift on operator-managed `NetworkPolicy` resources** (deletion, hand-edit). The watch + reconcile loop restores the desired state within seconds; re-created membership policies also emit a `PolicyRestored` Event.
- **Misconfiguration via wrong namespace.** Pods that try to join a vnet from a non-permitted namespace get a `VirtualNetworkNotJoinable` event and appear as `InvalidJoiners` on the vnet's `Degraded` condition rather than silently failing.
- **Forged membership.** The `kube-vnet.system/*` labels the policies select on are admission-protected: by a `ValidatingAdmissionPolicy` on Kubernetes ≥ 1.30, and for pods by the validating webhook when `webhook.enabled=true`.
- **Cross-namespace surprise.** `allowedNamespaces` is explicit; foreign namespaces don't get to join unless the vnet says so.

### What kube-vnet does NOT defend against

Be clear-eyed about these. None of them are bugs in kube-vnet; they're either out of scope or limitations of stock `NetworkPolicy`.

- **Egress exfiltration / lateral probing from a compromised pod.** kube-vnet's deny-all baseline restricts *ingress* only ([ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md)); pods can still initiate outbound traffic to anywhere they can route — other namespaces, the cluster control plane, the public internet. The vnet abstraction defends against unauthorized inbound; protecting against outbound exfiltration / lateral probing is a separate concern that needs separate tooling (a per-workload user-managed `NetworkPolicy` with `policyTypes: [Egress]`, a CNI egress firewall like Calico GlobalNetworkPolicy or Cilium FQDN policy, a NAT-gateway egress allowlist, or a service-mesh egress proxy).
- **Cluster admin compromise.** Anyone with cluster-admin (or with permissions to delete VirtualNetworks, edit the operator's RBAC, or stop the operator Deployment) can defeat kube-vnet entirely.
- **Namespace owner deleting the deny baseline.** A user with `delete networkpolicy` RBAC in their namespace can `kubectl delete networkpolicy kube-vnet.base`. The operator restores it within seconds (drift correction; see [`architecture.md`](../internals/architecture.md#drift-correction)), without an Event; during the window, traffic the policy would have denied is allowed. For a hard guarantee, the proper Kubernetes tool is `AdminNetworkPolicy` — see [ADR 0019](../adr/0019-baseline-durability.md).
- **CNI bypass.** kube-vnet generates `NetworkPolicy` resources; the CNI is what enforces them. If your CNI doesn't enforce `NetworkPolicy` (or if a pod manages to bypass the CNI — e.g. host-network pods), kube-vnet's policies have no effect.
- **Layer 7 / DNS / mTLS-identity policy.** kube-vnet emits L3/L4 `NetworkPolicy`. Anything HTTP-method-aware, hostname-aware, or identity-aware is out of scope; that's a service-mesh or CNI-extension responsibility.
- **In-pod traffic.** Containers within a single pod share a network namespace and are not policy-able by Kubernetes.
- **Kernel-level escapes.** A container that escapes the kernel's network namespace boundary is a kernel CVE, not a kube-vnet concern.

### Recommended companion: per-workload egress allowlists

For workloads where outbound restriction matters, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` selecting the workload's pods and listing the destinations they're permitted to reach. NetworkPolicies compose additively; nothing in kube-vnet conflicts with this. See the [per-workload egress allowlist recipe](../guides/recipes.md#per-workload-egress-allowlist-via-user-managed-networkpolicy).

---

## RBAC inventory

The operator runs as its own ServiceAccount in the release namespace (`<release>` in the chart, `kube-vnet-controller` in the kustomize install). The ClusterRole is generated from the `+kubebuilder:rbac` markers into `config/rbac/role.yaml`; the chart's copy is drift-tested against it.

### Cluster-scoped ClusterRole + ClusterRoleBinding

| API group | Resource | Verbs | Why |
|---|---|---|---|
| `networking.k8s.io` | `networkpolicies` | create, delete, get, list, patch, update, watch | The operator's output: baselines, membership and auto-allow policies in every managed namespace. |
| `""` | `pods` | get, list, watch, patch, update | Resolution stamps `kube-vnet.system/*` labels and annotations; membership and hostPort discovery. |
| `""` | `namespaces`, `services` | get, list, watch | Managed/disabled state, `allowedNamespaces.selector`, opt-out annotations; auto-allow triggers. |
| `""`, `events.k8s.io` | `events` | create, patch | [Events](../reference/metrics-and-events.md#kubernetes-events). |
| `kube-vnet.lhns.de` | `virtualnetworks` | create, delete, get, list, patch, update, watch | Reconciling vnets; creating and removing the `namespace` / `cluster` system vnets. |
| `kube-vnet.lhns.de` | `virtualnetworkbindings`, `virtualnetworkbaselines`, `clustervirtualnetworkbaselines` | get, list, watch | Resolution inputs. |
| `kube-vnet.lhns.de` | `*/status` of all four CRDs | get, patch, update | Status writes (today only vnets and bindings are written). |
| `kube-vnet.lhns.de` | `virtualnetworks/finalizers`, `virtualnetworkbindings/finalizers` | update | kubebuilder convention; no finalizers are set today. |
| `admissionregistration.k8s.io` | `validatingwebhookconfigurations`, `mutatingwebhookconfigurations` | get, list, watch | `ext.apiserver` discovery. |
| `apiregistration.k8s.io` | `apiservices` | get, list, watch | `ext.apiserver` discovery. |
| `apiextensions.k8s.io` | `customresourcedefinitions` | get, list, watch | `ext.apiserver` discovery (conversion webhooks). |

### Namespace-scoped Role + RoleBinding (operator namespace only)

| API group | Resource | Verbs | Why |
|---|---|---|---|
| `coordination.k8s.io` | `leases` | get, list, watch, create, update, patch, delete | Leader election. The lease object is `kube-vnet.lhns.de`. |
| `""` | `events` | create, patch | controller-runtime emits leader-election events. |

### Uninstall hook (chart only, transient)

The `pre-delete` cleanup Job runs as `<release>-cleanup` with a ClusterRole granting `networkpolicies` list/delete/deletecollection, `deployments` get/delete and `pods` get/list/watch, cluster-wide. It exists only during `helm uninstall` ([ADR 0036](../adr/0036-helm-pre-delete-hook-cleanup.md), [threat model F-01](threat-model.md#7-findings-register)).

### What this means for blast radius

The operator can create, modify, or delete any `NetworkPolicy` and relabel any Pod — together a total isolation bypass if its token is stolen ([threat model F-10](threat-model.md#7-findings-register)). It cannot read Secrets or ConfigMaps, cannot create or delete Pods, and cannot write Namespaces, RBAC, or anything outside the table above.

### Who can write what

The chart ships ClusterRoles aggregated into the upstream `admin`/`edit`/`view` defaults so the right authority tier writes the right CRD. With `rbac.aggregate=true` (default):

| Kind | Default writer | Mechanism |
|---|---|---|
| `VirtualNetwork` | namespace-admin | `<release>-virtualnetworks-editor` ClusterRole, aggregated into `admin`+`edit` |
| `VirtualNetworkBinding` | namespace-admin | `<release>-virtualnetworkbindings-editor`, aggregated into `admin`+`edit` |
| `VirtualNetworkBaseline` | namespace-admin | `<release>-virtualnetworkbaselines-editor`, aggregated into `admin`+`edit` |
| `ClusterVirtualNetworkBaseline` | cluster-admin | no aggregation; `<release>-clustervirtualnetworkbaselines-editor` is shipped **unbound** so cluster-admins can delegate via their own `ClusterRoleBinding` |

A matching `*-viewer` ClusterRole per CRD aggregates into `view` (or, for the cluster baseline, ships unbound).

The cluster baseline is a high-leverage knob — editing it changes every namespace's default ingress posture. Bind the editor role only to identities that already need cluster-wide policy authority. The reserved-name VAP (Kubernetes ≥ 1.30) prevents namespace-admins from sneaking around this by creating a `VirtualNetwork` named `cluster` or carrying `kube-vnet.system/managed-by=kube-vnet`.

To skip the chart-shipped end-user RBAC entirely (managing it externally via Argo, GitOps, etc.), set `rbac.aggregate=false`.

---

## Supply chain

### Image and chart signing (cosign keyless)

Released images and the OCI Helm chart are signed with [Cosign](https://docs.sigstore.dev/cosign/) keyless, using the GitHub OIDC token — no long-lived keys. The signing identity is the release workflow: `https://github.com/lhns/kube-vnet/.github/workflows/release.yaml@refs/tags/<tag>`. A successful verify proves the artifact was built and pushed by that workflow; if it fails, do not deploy.

Verify commands: [`install.md` § Verifying signatures](../getting-started/install.md#verifying-signatures).

### SBOMs

Every release ships SPDX-JSON SBOMs for both the image and the chart. They're attached as Cosign attestations *and* as plain GitHub release assets. See [`install.md`](../getting-started/install.md#verifying-sboms).

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

- The operator container exposes:
  - `:8080` — Prometheus metrics. Not exposed via a Service by default; opt in with `metricsService.enabled=true` or `podMonitor.enabled=true`.
  - `:8081` — health/readiness probes.
  - `:9443` — only with `webhook.enabled=true`: the admission webhook server, reached by the apiserver through the `<release>-webhook` Service (TLS; certificate from the chart or cert-manager).
- It makes egress only to the apiserver (and to CoreDNS for resolution).
- The release namespace is disabled for the operator itself, so no kube-vnet policy restricts ingress to it.

### Identity / authentication

- The operator authenticates to the apiserver via its `ServiceAccount` token (the standard projected-volume mechanism).
- Leader election uses the same identity to update the lease.
- No external secrets, no service mesh dependency.

---

## Common security questions

### Can the operator be locked down further by removing some permissions?

Not without losing functionality. Each line in the RBAC inventory above maps to a feature. The most-asked-about removal is `networkpolicies` cluster-wide write — but the baseline goes into every managed namespace and cross-namespace vnets put membership policies into foreign namespaces; scoping it per namespace would mean one operator per namespace, which defeats the design.

### Can a namespace owner permanently disable kube-vnet for their namespace?

Yes — by annotating the namespace `kube-vnet/disabled: "true"`. This removes the operator's baseline and any membership policies; pods in the namespace are not eligible joiners for any vnet. See [ADR 0006](../adr/0006-baseline-default-deny-and-single-opt-out.md).

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

Adoption is deferred for now (CNI support is still maturing across the ecosystem; the API itself is `v1alpha1`/`v1beta1` depending on version). Drift correction is sufficient for the dominant threat (accidental deletion or unaware tooling). When ANP support is broad enough, the deny baseline migrates to a single cluster-scoped ANP; per-vnet allow policies stay as `NetworkPolicy`.

Full discussion: [ADR 0019](../adr/0019-baseline-durability.md).

---

## Reporting a vulnerability

See [`SECURITY.md`](../../SECURITY.md): report privately through GitHub's security advisories, not in a public issue.
