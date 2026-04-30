# Glossary

Defined terms used throughout the kube-vnet documentation.

---

**`allowedNamespaces`** — a field on `VirtualNetwork.spec` that controls which namespaces' pods may *join* the network. Three matchers — `All` (wildcard), `Names` (exact list), `Selector` (label-based) — that union. The home namespace is always implicitly included. Does *not* grant blanket access; pods in permitted namespaces still need the join label. See [ADR 0005](adr/0005-namespaced-crd-with-allowed-namespaces.md).

**AdminNetworkPolicy (ANP)** — a `policy.networking.k8s.io/v1` resource. Cluster-scoped, distinct RBAC from `NetworkPolicy`, higher precedence. Tracked as the future direction for kube-vnet's deny baseline; currently deferred — see [ADR 0019](adr/0019-baseline-durability.md).

**Bare label form** — a join label without a namespace prefix: `kube-vnet/net.<vnet-name>=true`. Used by pods *in the VirtualNetwork's home namespace*. Compare *prefixed label form*.

**Baseline (default-deny baseline)** — the `NetworkPolicy` named `kube-vnet-default-deny` that the operator installs in any managed namespace where at least one pod is a member of any vnet. Empty `podSelector` (selects all pods), `policyTypes: [Ingress, Egress]`, no allow rules except DNS egress to CoreDNS. See [ADR 0006](adr/0006-baseline-default-deny-and-single-opt-out.md).

**CNI** — Container Network Interface. The networking plugin running in a Kubernetes cluster (Calico, Cilium, kube-router, Antrea, etc.). kube-vnet generates `NetworkPolicy` resources; the CNI is what enforces them by dropping packets.

**Cluster-scoped resource** — a Kubernetes resource that lives outside any namespace (e.g. `Node`, `ClusterRole`, `AdminNetworkPolicy`, the `VirtualNetwork` CRD definition itself). Compare *namespaced resource*.

**Drift correction** — the operator's mechanism for restoring its `NetworkPolicy` resources after they're deleted or hand-edited out-of-band. Triggered by NetworkPolicy watch events; restored via server-side apply on the next reconcile.

**Excluded namespace** — a namespace the operator explicitly does nothing in. Excluded via the `--excluded-namespaces` flag (defaults: `kube-system`, `kube-public`, `kube-node-lease`, plus the operator's own namespace) or per-namespace via `kube-vnet/disabled=true` annotation. See ADRs [0006](adr/0006-baseline-default-deny-and-single-opt-out.md) and [0007](adr/0007-operator-level-excluded-namespaces.md).

**Field manager** — a name kube-vnet uses for server-side apply (`kube-vnet`). Tracks which fields it owns; combined with `client.ForceOwnership` enables drift correction. See [ADR 0009](adr/0009-server-side-apply-with-field-manager.md).

**Foreign namespace** — a namespace other than a VirtualNetwork's home namespace. Pods in foreign namespaces use the *prefixed label form* to join.

**Generator** — the pure function in `internal/controller/policy_generator.go:Generate` that takes a VirtualNetwork plus its member set and returns the desired `NetworkPolicy` slice. No client, no I/O. See [ADR 0008](adr/0008-pure-function-policy-generator.md).

**Home namespace** — the namespace a `VirtualNetwork` resource lives in. Pods in the home namespace can join with the bare label form; the home namespace is always implicitly in `allowedNamespaces`.

**InvalidJoiner** — a pod that carries the prefixed join label but lives in a non-permitted namespace (excluded, disabled-annotated, or not in `allowedNamespaces`). Surfaced on the VirtualNetwork's `Degraded` condition with reason `InvalidJoiners` and a per-pod sub-reason (`NamespaceExcluded`, `NamespaceNotAllowed`).

**Join label** — a label on a pod that declares membership in a `VirtualNetwork`. One label per joined network. The operator only checks key presence; values are conventionally `"true"` but the value is not inspected. Two forms: bare (`kube-vnet/net.<vnet>`) for same-namespace, prefixed (`kube-vnet/net.<homeNS>.<vnet>`) for cross-namespace.

**Leader election** — the mechanism that ensures only one operator replica is actively reconciling. Implemented via a `coordination.k8s.io/v1 Lease` named `kube-vnet.lhns.de` in the operator's namespace. See [`operations.md` § Leader election semantics](operations.md#leader-election-semantics).

**Managed namespace** — a namespace the operator does act in. The opposite of an *excluded namespace*. Determined by `NamespaceFilter.IsManaged(ns)`.

**Member** — a pod that is in a VirtualNetwork. Listed in `VirtualNetwork.status.members`. Selected by the generated membership policy via `Exists` on the appropriate join key.

**Membership policy** — the per-vnet, per-namespace `NetworkPolicy` named `kube-vnet-<vnet>-<ns>` that the operator generates. Selects members via `Exists` on the join key; allows ingress from / egress to other members across all member-bearing namespaces.

**`NamespaceReconciler`** — the controller-runtime reconciler in `internal/controller/namespace_reconciler.go` that watches `corev1.Namespace` and (when `--default-deny-everywhere` is on) installs the baseline in every managed namespace. Owns the *flag-driven* baseline lifecycle.

**Namespaced resource** — a Kubernetes resource that lives in a namespace (`Pod`, `NetworkPolicy`, `VirtualNetwork`). Compare *cluster-scoped resource*.

**`NetworkPolicy`** — the standard `networking.k8s.io/v1` resource for L3/L4 pod-level network policy. Namespace-local in what it selects but can reference pods in other namespaces via peer rules. The thing kube-vnet generates.

**Operator** — kube-vnet's controller. Runs as a single Deployment in the `kube-vnet-system` namespace (the conventional install location). Reconciles `VirtualNetwork` resources into `NetworkPolicy` resources.

**Owner reference** — a Kubernetes metadata field that establishes a parent-child relationship for garbage collection. kube-vnet sets owner references on policies in the home namespace only; cross-namespace owner references are unsupported by Kubernetes. See [ADR 0010](adr/0010-cross-namespace-cleanup-via-network-label.md).

**Peer rule** — an entry in `NetworkPolicy.spec.ingress[].from` or `egress[].to`. Each peer can be a `namespaceSelector + podSelector` referencing pods in other namespaces. kube-vnet's generated peers always restrict pods to those carrying the appropriate join key (`Exists` selector).

**Prefixed label form** — a join label with the home namespace baked into the key: `kube-vnet/net.<homeNS>.<vnet-name>=true`. Used by pods *in any namespace other than the VirtualNetwork's home namespace*. Compare *bare label form*.

**Reconciler** — a controller-runtime component that drives a resource toward its desired state on every event. kube-vnet has two: `VirtualNetworkReconciler` (per-vnet) and `NamespaceReconciler` (per-namespace, flag-driven).

**SBOM** — Software Bill of Materials. SPDX-JSON formatted list of every dependency in a built artifact. kube-vnet ships SBOMs for both the image and the Helm chart, attached as Cosign attestations and as plain release assets. See [`security.md`](security.md#sboms).

**Server-side apply (SSA)** — a Kubernetes apiserver feature where the client sends a partial object and the server reconciles per-field ownership. kube-vnet uses SSA with `FieldOwner("kube-vnet")` and `client.ForceOwnership` for all generated `NetworkPolicy` writes. See [ADR 0009](adr/0009-server-side-apply-with-field-manager.md).

**vnet** — short for `VirtualNetwork`. Also the CRD's short name (`kubectl get vnet`).

**`VirtualNetwork`** — the kube-vnet CRD. A named, namespaced resource. Pods join it by adding a label; same-network pods can talk to each other.

**`VirtualNetworkReconciler`** — the controller-runtime reconciler in `internal/controller/virtualnetwork_controller.go` that watches `VirtualNetwork`, `Pod`, and `NetworkPolicy`. The primary reconciler; owns membership-driven baselines and all per-vnet membership policies.
