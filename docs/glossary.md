# Glossary

Defined terms used throughout the kube-vnet documentation.

---

**`allowedNamespaces`** — a field on `VirtualNetwork.spec` that controls which namespaces' pods may *join* the network. Three matchers — `All` (wildcard), `Names` (exact list), `Selector` (label-based) — that union. The home namespace is always implicitly included. Does *not* grant blanket access; pods in permitted namespaces still need the join label. See [ADR 0005](adr/0005-namespaced-crd-with-allowed-namespaces.md).

**AdminNetworkPolicy (ANP)** — a `policy.networking.k8s.io/v1` resource. Cluster-scoped, distinct RBAC from `NetworkPolicy`, higher precedence. Tracked as the future direction for kube-vnet's deny baseline; currently deferred — see [ADR 0019](adr/0019-baseline-durability.md).

**Bare label form** — a join label without a namespace prefix: `kube-vnet/net.<vnet-name>=<direction>`. Used by pods *in the VirtualNetwork's home namespace*. (Pods in the home namespace may also use the prefixed form — see [ADR 0022](adr/0022-long-form-join-label-in-home-namespace.md).) Compare *prefixed label form*.

**Baseline** — the `NetworkPolicy` named `kube-vnet` that the operator installs in every managed namespace. `policyTypes: [Ingress]`, no allow rules → deny-all ingress. The `podSelector` excludes pods that are receivers (direction `both` or `ingress`) on any vnet listed in `--elide-baseline-for` (default `cluster`); all other pods are selected and denied. Egress is unrestricted by the baseline. Owned by the `NamespaceReconciler`. See [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md).

**Binding** — short for `VirtualNetworkBinding`. Also the CRD's short name (`kubectl get vnb`).

**CNI** — Container Network Interface. The networking plugin running in a Kubernetes cluster (Calico, Cilium, kube-router, Antrea, etc.). kube-vnet generates `NetworkPolicy` resources; the CNI is what enforces them by dropping packets.

**Cluster-scoped resource** — a Kubernetes resource that lives outside any namespace (e.g. `Node`, `ClusterRole`, `AdminNetworkPolicy`, the `VirtualNetwork` CRD definition itself). Compare *namespaced resource*.

**Direction** — the value carried by a join label, declaring which directions the labeled pod participates in. Exactly one of `both` (default), `ingress`, `egress`, `none`. The legacy `"true"`/`"false"`/empty-string aliases were dropped per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md) (see also the [ADR 0021 2026-05-05 addendum](adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-05--legacy-truefalseempty-aliases-dropped)). Unknown values surface as `Degraded`/`UnknownDirection`. Traffic algebra: X→Y iff X is initiator-capable (`both`/`egress`) AND Y is receiver-capable (`both`/`ingress`).

**Drift correction** — the operator's mechanism for restoring its `NetworkPolicy` resources after they're deleted or hand-edited out-of-band. Triggered by NetworkPolicy watch events; restored via server-side apply on the next reconcile.

**Disabled namespace** — a namespace the operator explicitly does nothing in. Disabled via the `--disabled-namespaces` flag / `operator.disabledNamespaces` Helm value (default: `kube-system`, `kube-public`, `kube-node-lease`, plus the operator's own namespace added implicitly) or per-namespace via the `kube-vnet/disabled=true` annotation. See ADRs [0006](adr/0006-baseline-default-deny-and-single-opt-out.md), [0007](adr/0007-operator-level-excluded-namespaces.md), and [0030](adr/0030-unified-vnet-membership-with-resolution.md).

**Field manager** — a name kube-vnet uses for server-side apply (`kube-vnet`). Tracks which fields it owns; combined with `client.ForceOwnership` enables drift correction. See [ADR 0009](adr/0009-server-side-apply-with-field-manager.md).

**Foreign namespace** — a namespace other than a VirtualNetwork's home namespace. Pods in foreign namespaces use the *prefixed label form* to join.

**Generator** — the pure function in `internal/controller/policy_generator.go:Generate` that takes a VirtualNetwork plus its member set and returns the desired `NetworkPolicy` slice. No client, no I/O. See [ADR 0008](adr/0008-pure-function-policy-generator.md).

**Home namespace** — the namespace a `VirtualNetwork` resource lives in. Pods in the home namespace can join with either the bare or the prefixed label form; the home namespace is always implicitly in `allowedNamespaces`.

**Ingress isolation** *(historical)* — under earlier ADRs the per-namespace baseline shape was selected by a `kube-vnet/ingress-isolation` annotation with values `none`/`namespace`/`pod`. [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md) replaced that with a uniform deny-all baseline plus a configurable `--elide-baseline-for` exemption list driven by per-pod system-vnet membership. The annotation, the `IsolationMode` enum, and the `--ingress-isolation*` flags are gone.

**InvalidJoiner** — a pod that carries the prefixed join label but lives in a non-permitted namespace (excluded, disabled-annotated, or not in `allowedNamespaces`). Surfaced on the VirtualNetwork's `Degraded` condition with reason `InvalidJoiners` and a per-pod sub-reason (`NamespaceExcluded`, `NamespaceNotAllowed`).

**Join label** — a user-authored label on a pod declaring membership in a `VirtualNetwork`. One label per joined network. The label *value* is exactly one of the four directions (`both`/`ingress`/`egress`/`none`). Two forms: bare (`kube-vnet/net.<vnet>`) — accepted in the home namespace; prefixed (`kube-vnet/net.<homeNS>.<vnet>`) — accepted in any namespace, required for foreign namespaces. Inputs to the resolution layer (see *System label*); not directly read by the policy generator.

**Leader election** — the mechanism that ensures only one operator replica is actively reconciling. Implemented via a `coordination.k8s.io/v1 Lease` named `kube-vnet.lhns.de` in the operator's namespace. See [`operations.md` § Leader election semantics](operations.md#leader-election-semantics).

**Managed namespace** — a namespace the operator does act in. The opposite of an *excluded namespace*. Determined by `NamespaceFilter.IsManaged(ns)`.

**Member** — a pod that is in a VirtualNetwork. Listed in `VirtualNetwork.status.members`. Selected by the generated membership policy via `Exists` on the appropriate join key.

**Membership policy** — the per-vnet, per-namespace `NetworkPolicy` the operator generates. Names follow the dot-separated scheme `kube-vnet-<vnet>-<8hex>` (bare), `kube-vnet-<homeNS>.<vnet>-<8hex>` (prefixed), or `kube-vnet-<homeNS>.<vnet>.b.<binding>-<8hex>` (binding-driven) — see [ADR 0011 + naming addendums]. Selects members via the `kube-vnet.system/net.<vnet>` label (operator-stamped); allows ingress from other members across all member-bearing namespaces. Ingress-only ([ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md)).

**`NamespaceReconciler`** — the controller-runtime reconciler in `internal/controller/namespace_reconciler.go` that watches `corev1.Namespace` and applies the deny-all baseline (with `--elide-baseline-for` exemptions) to every managed namespace. **Sole owner** of the baseline lifecycle. See [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md).

**Namespaced resource** — a Kubernetes resource that lives in a namespace (`Pod`, `NetworkPolicy`, `VirtualNetwork`). Compare *cluster-scoped resource*.

**`NetworkPolicy`** — the standard `networking.k8s.io/v1` resource for L3/L4 pod-level network policy. Namespace-local in what it selects but can reference pods in other namespaces via peer rules. The thing kube-vnet generates.

**Operator** — kube-vnet's controller. Runs as a single Deployment in the `kube-vnet-system` namespace (the conventional install location). Reconciles `VirtualNetwork` resources into `NetworkPolicy` resources.

**Owner reference** — a Kubernetes metadata field that establishes a parent-child relationship for garbage collection. kube-vnet sets owner references on policies in the home namespace only; cross-namespace owner references are unsupported by Kubernetes. See [ADR 0010](adr/0010-cross-namespace-cleanup-via-network-label.md).

**Peer rule** — an entry in `NetworkPolicy.spec.ingress[].from` or `egress[].to`. Each peer can be a `namespaceSelector + podSelector` referencing pods in other namespaces. kube-vnet's generated peers always restrict pods to those carrying the appropriate join key (`Exists` selector).

**Prefixed label form** — a join label with the home namespace baked into the key: `kube-vnet/net.<homeNS>.<vnet-name>=true`. Used by pods *in any namespace other than the VirtualNetwork's home namespace*. Compare *bare label form*.

**Reconciler** — a controller-runtime component that drives a resource toward its desired state on every event. kube-vnet has five: `VirtualNetworkReconciler` (per-vnet membership policies), `NamespaceReconciler` (per-namespace baseline lifecycle), `VirtualNetworkBindingReconciler` (per-binding status), `SystemVnetReconciler` (operator-managed `namespace`/`cluster` system vnets — [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md)), and `ResolutionReconciler` (computes effective vnet membership per pod and stamps `kube-vnet.system/net.<vnet>` labels — [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md)).

**SBOM** — Software Bill of Materials. SPDX-JSON formatted list of every dependency in a built artifact. kube-vnet ships SBOMs for both the image and the Helm chart, attached as Cosign attestations and as plain release assets. See [`security.md`](security.md#sboms).

**Server-side apply (SSA)** — a Kubernetes apiserver feature where the client sends a partial object and the server reconciles per-field ownership. kube-vnet uses SSA with `FieldOwner("kube-vnet")` and `client.ForceOwnership` for all generated `NetworkPolicy` writes. See [ADR 0009](adr/0009-server-side-apply-with-field-manager.md).

**System label** — an operator-stamped label of the form `kube-vnet.system/net.<vnet>=<direction>`. Written by the `ResolutionReconciler` based on the inheritance lattice (cluster baseline → namespace baseline → pod tier where bindings and pod labels intersect). Read by the policy generator's selectors. Protected from user mutation by a ValidatingAdmissionPolicy. See [ADR 0031](adr/0031-baseline-tier-resolution.md).

**System vnet** — an operator-managed `VirtualNetwork` resource labeled `kube-vnet/system=true`. Two of them: `namespace` (one per managed namespace) and `cluster` (one in the operator's namespace, with `allowedNamespaces.All=true`). Drift-corrected by the `SystemVnetReconciler`; protected from user mutation by a ValidatingAdmissionPolicy. See [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md).

**vnet** — short for `VirtualNetwork`. Also the CRD's short name (`kubectl get vnet`).

**`VirtualNetwork`** — the kube-vnet CRD. A named, namespaced resource. Pods join it by adding a label (or via a `VirtualNetworkBinding`); same-network pods can talk to each other in the directions their labels declare.

**`VirtualNetworkBinding`** — a namespaced CRD (short names `vnb`, `vnbs`) that selects pods *in its own namespace* via a `podSelector` and attaches them to a target `VirtualNetwork` for a chosen `direction`. The escape hatch for enrolling pods whose template you can't modify (third-party Helm charts, other operators). `spec.virtualNetworkRef.{name,namespace}` names the target; the target vnet's `spec.allowedNamespaces` is enforced. Status: `Ready` condition with reasons `PodsAttached`, `NoPodsMatch`, `VirtualNetworkNotFound`, `NamespaceNotAllowed`, `NamespaceExcluded`, `UnknownDirection`, `InvalidSelector`; plus `attachedPods` and `observedGeneration`. Generated policies named `kube-vnet-<vnet>-b-<binding>` and labeled `kube-vnet/binding=<binding>`. See [ADR 0026](adr/0026-virtualnetworkbinding-crd.md).

**`VirtualNetworkReconciler`** — the controller-runtime reconciler in `internal/controller/virtualnetwork_controller.go` that watches `VirtualNetwork`, `Pod`, `NetworkPolicy`, and `VirtualNetworkBinding`. Generates per-vnet membership policies (including binding-driven ones). Does **not** own the baseline lifecycle (that's the `NamespaceReconciler`).

**`ClusterVirtualNetworkBaseline`** — a cluster-scoped singleton CRD (short names `cvnbl`, `cvnbls`) introduced in [ADR 0031](adr/0031-baseline-tier-resolution.md). Named `default` (CEL-pinned). Carries the cluster-wide tier-default vnet memberships. Direction values are bare (enforced) or `default-*` (override-permitted by lower tiers). The chart seeds it from `operator.clusterBaseline.{ingressIsolationLevel, memberships}`.

**`VirtualNetworkBaseline`** — a namespace-scoped singleton CRD (short names `vnbl`, `vnbls`) introduced in [ADR 0031](adr/0031-baseline-tier-resolution.md). Named `default` per namespace (CEL-pinned). Carries namespace-wide tier-default memberships; can override `default-*` cluster-baseline entries.

**`ResolutionReconciler`** — the controller-runtime reconciler in `internal/controller/resolution_controller.go` that watches `Pod`, `ClusterVirtualNetworkBaseline`, `VirtualNetworkBaseline`, and `VirtualNetworkBinding`. Computes the effective `(vnet, direction)` per pod via the three-tier inheritance lattice (cluster baseline → namespace baseline → pod tier; intersection on within-tier conflicts) and patches `kube-vnet.system/net.<vnet>=<direction>` labels onto pods. See [ADR 0031](adr/0031-baseline-tier-resolution.md).

**`SystemVnetReconciler`** — the controller-runtime reconciler in `internal/controller/system_vnet_controller.go` that ensures the operator-managed `namespace` and `cluster` *system vnets* exist in every managed namespace and the operator namespace, respectively. Drift-corrects deletes. See [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md).
