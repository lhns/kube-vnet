# Glossary

Defined terms used throughout the kube-vnet documentation.

---

**AdminNetworkPolicy (ANP)** — a `policy.networking.k8s.io` resource (not GA). Cluster-scoped, distinct RBAC from `NetworkPolicy`, higher precedence. Tracked as the future direction for kube-vnet's deny baseline; currently deferred — see [ADR 0019](../adr/0019-baseline-durability.md).

**Admission webhook** — the optional pod-resolution webhook pair (`webhook.enabled`, [ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md)). The mutating half stamps *system labels* during pod admission; the validating half rejects writes whose system labels disagree with resolution, and, for the pods it sees, takes over from the system-labels `ValidatingAdmissionPolicy`, which keeps covering the pods the webhooks skip.

**`allowedNamespaces`** — a field on `VirtualNetwork.spec` that controls which namespaces' pods may *join* the network. Three matchers — `All` (wildcard), `Names` (exact list), `Selector` (label-based) — that union. The home namespace is always implicitly included. Does *not* grant blanket access; pods in permitted namespaces still need the join label. See [ADR 0005](../adr/0005-namespaced-crd-with-allowed-namespaces.md).

**Bare label form** — a join label without a namespace prefix: `kube-vnet/net.<vnet-name>=<direction>`. Used by pods *in the VirtualNetwork's home namespace* (for the system vnets, in any managed namespace). (Pods in the home namespace may also use the prefixed form — see [ADR 0022](../adr/0022-long-form-join-label-in-home-namespace.md).) Compare *prefixed label form*.

**Baseline** — the `NetworkPolicy` named `kube-vnet.base` (per [ADR 0039](../adr/0039-uniform-kind-prefixed-policy-naming.md)) that the operator installs in every managed namespace. `policyTypes: [Ingress]`, no allow rules → deny-all ingress. `podSelector: {}` selects every pod in the namespace; vnet members get additive allows via their membership policies (per NetworkPolicy union, those allows override the baseline's deny-all). Egress is unrestricted by the baseline. Owned by the `NamespaceReconciler`. See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) and [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md).

**Binding** — short for `VirtualNetworkBinding`. Also the CRD's short name (`kubectl get vnb`).

**Cluster-scoped resource** — a Kubernetes resource that lives outside any namespace (e.g. `Node`, `ClusterRole`, `AdminNetworkPolicy`, the `VirtualNetwork` CRD definition itself). Compare *namespaced resource*.

**`ClusterVirtualNetworkBaseline`** — a cluster-scoped singleton CRD (short names `cvnbl`, `cvnbls`) introduced in [ADR 0031](../adr/0031-baseline-tier-resolution.md). Named `default` (CEL-pinned). Carries the cluster-wide tier-default vnet memberships. Direction values are bare (enforced) or `default-*` (override-permitted by lower tiers). The chart seeds it from `operator.clusterBaseline.{ingressIsolationLevel, memberships}`.

**CNI** — Container Network Interface. The networking plugin running in a Kubernetes cluster (Calico, Cilium, kube-router, Antrea, etc.). kube-vnet generates `NetworkPolicy` resources; the CNI is what enforces them by dropping packets.

**Direction** — the value carried by a join label, declaring which directions the labeled pod participates in. At the pod tier (labels, bindings): exactly one of `both`, `ingress`, `egress`, `none` (a binding's `direction` defaults to `both`). Baseline tiers additionally accept `default-both`/`default-ingress`/`default-egress`/`default-none` — advisory values lower tiers may override (bare values are enforced; see [ADR 0031](../adr/0031-baseline-tier-resolution.md)). The legacy `"true"`/`"false"`/empty-string aliases were dropped per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) (see also the [ADR 0021 2026-05-05 addendum](../adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-05--legacy-truefalseempty-aliases-dropped)). Unknown label values are rejected at admission (Kubernetes ≥ 1.30) or ignored with an `InvalidJoinLabelDirection` event. Traffic algebra: X→Y iff X is initiator-capable (`both`/`egress`) AND Y is receiver-capable (`both`/`ingress`).

**Disabled namespace** — a namespace the operator explicitly does nothing in. Disabled via the `--disabled-namespaces` flag / `operator.disabledNamespaces` Helm value (default: `kube-system`, plus the operator's own namespace added implicitly) or per-namespace via the `kube-vnet/disabled=true` annotation. See ADRs [0006](../adr/0006-baseline-default-deny-and-single-opt-out.md), [0007](../adr/0007-operator-level-excluded-namespaces.md), and [0030](../adr/0030-unified-vnet-membership-with-resolution.md).

**Drift correction** — the operator's mechanism for restoring its `NetworkPolicy` resources and system vnets after they're deleted or hand-edited out-of-band. Triggered by watch events; restored via server-side apply.

**Field manager** — a name kube-vnet uses for server-side apply (`kube-vnet`). Tracks which fields it owns; combined with `client.ForceOwnership` enables drift correction. See [ADR 0009](../adr/0009-server-side-apply-with-field-manager.md).

**Foreign namespace** — a namespace other than a VirtualNetwork's home namespace. Pods in foreign namespaces use the *prefixed label form* to join.

**Generator** — the pure function in `internal/controller/policy_generator.go:Generate` that takes a VirtualNetwork plus its member set and returns the desired `NetworkPolicy` slice. No client, no I/O. See [ADR 0008](../adr/0008-pure-function-policy-generator.md).

**Home namespace** — the namespace a `VirtualNetwork` resource lives in. Pods in the home namespace can join with either the bare or the prefixed label form; the home namespace is always implicitly in `allowedNamespaces`.

**Ingress isolation** *(historical)* — under earlier ADRs the per-namespace baseline shape was selected by a `kube-vnet/ingress-isolation` annotation with values `none`/`namespace`/`pod`. [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) replaced that with a uniform deny-all baseline driven by per-pod system-vnet membership. The annotation, the `IsolationMode` enum, and the `--ingress-isolation*` flags are gone. ADR 0030 also introduced a `--elide-baseline-for` exemption list, since removed by [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md). The three modes survive as presets in [ADR 0031](../adr/0031-baseline-tier-resolution.md)'s `operator.clusterBaseline.ingressIsolationLevel` chart value.

**InvalidJoiner** — a pod whose join label for a vnet can't be honored: unknown direction value, disabled namespace, or namespace not in `allowedNamespaces`. Surfaced on the VirtualNetwork's `Degraded` condition with reason `InvalidJoiners` and a per-pod sub-reason (`UnknownDirection`, `NamespaceExcluded`, `NamespaceNotAllowed`).

**Join label** — a user-authored label on a pod declaring membership in a `VirtualNetwork`. One label per joined network. The label *value* is exactly one of the four directions (`both`/`ingress`/`egress`/`none`). Two forms: bare (`kube-vnet/net.<vnet>`) — accepted in the home namespace; prefixed (`kube-vnet/net.<homeNS>.<vnet>`) — accepted in any namespace, required for foreign namespaces. Inputs to the resolution layer (see *System label*); not directly read by the policy generator.

**Leader election** — the mechanism that ensures only one operator replica is actively reconciling. Implemented via a `coordination.k8s.io/v1 Lease` named `kube-vnet.lhns.de` in the operator's namespace. See [`operations.md` § Leader election semantics](../guides/operations.md#leader-election-semantics).

**Managed namespace** — a namespace the operator does act in. The opposite of a *disabled namespace*. Determined by `NamespaceFilter.IsManaged(ns)`.

**Member** — a pod that is in a VirtualNetwork, i.e. carries its *system label*. Listed in `VirtualNetwork.status.members`.

**Membership policy** — the per-vnet, per-namespace `NetworkPolicy` the operator generates. Named `kube-vnet.mem.<homeNS>.<vnet>-<8hex>` (`kube-vnet.mem.cluster-<8hex>` for the cluster system vnet) per [ADR 0039](../adr/0039-uniform-kind-prefixed-policy-naming.md); one policy per (vnet, member-bearing namespace) — label-driven and binding-driven members alike, no per-binding policy ([ADR 0033](../adr/0033-canonical-fq-system-labels.md)). Selects receiver-capable members via the stamped `kube-vnet.system/net.<homeNS>.<vnet>` label; allows ingress from initiator-capable members across all member namespaces. Ingress-only ([ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md)).

**Namespaced resource** — a Kubernetes resource that lives in a namespace (`Pod`, `NetworkPolicy`, `VirtualNetwork`). Compare *cluster-scoped resource*.

**`NamespaceReconciler`** — the controller-runtime reconciler in `internal/controller/namespace_reconciler.go` that watches `corev1.Namespace` and applies the uniform deny-all baseline to every managed namespace. **Sole owner** of the baseline lifecycle. See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) and [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md).

**Network wait** — the optional init container (`kube-vnet-network-wait`) the admission webhook injects into a pod annotated `kube-vnet/network-max-wait`. It holds the app until every node's per-node beacon accepts the pod, i.e. every node's CNI has applied its rules, or until the maximum runs out. Needs `webhook.networkWait.enabled`. See [labels and annotations](labels-and-annotations.md#kube-vnetnetwork-max-wait) and [ADR 0045](../adr/0045-network-wait-for-opted-in-pods.md).

**`NetworkPolicy`** — the standard `networking.k8s.io/v1` resource for L3/L4 pod-level network policy. Namespace-local in what it selects but can reference pods in other namespaces via peer rules. The thing kube-vnet generates.

**Operator** — kube-vnet's controller. Runs as a single Deployment in the `kube-vnet-system` namespace (the conventional install location). Reconciles `VirtualNetwork` resources into `NetworkPolicy` resources.

**Owner reference** — a Kubernetes metadata field that establishes a parent-child relationship for garbage collection. kube-vnet sets owner references on policies in the home namespace only; cross-namespace owner references are unsupported by Kubernetes. See [ADR 0010](../adr/0010-cross-namespace-cleanup-via-network-label.md).

**Peer rule** — an entry in `NetworkPolicy.spec.ingress[].from` or `egress[].to`. Each peer can be a `namespaceSelector + podSelector` referencing pods in other namespaces. kube-vnet's generated peers are `ingress.from` entries selecting the vnet's *system label* with `In [both, egress]`.

**Prefixed label form** — a join label with the home namespace baked into the key: `kube-vnet/net.<homeNS>.<vnet-name>=<direction>` (e.g. `=both`). Required outside the VirtualNetwork's home namespace; also accepted in it. Compare *bare label form*.

**Reconciler** — a controller-runtime component that drives a resource toward its desired state on every event. kube-vnet has eight; the list and what each owns is in [architecture](../internals/architecture.md#high-level).

**`ResolutionReconciler`** — the reconciler in `internal/controller/resolution_controller.go` that computes the effective `(vnet, direction)` per pod via the three-tier inheritance lattice (cluster baseline → namespace baseline → pod tier; intersection on within-tier conflicts) and stamps the *system labels*. See [ADR 0031](../adr/0031-baseline-tier-resolution.md).

**SBOM** — Software Bill of Materials. SPDX-JSON formatted list of every dependency in a built artifact. kube-vnet ships SBOMs for both the image and the Helm chart, attached as Cosign attestations and as plain release assets. See [`security.md`](../security/security.md#sboms).

**Server-side apply (SSA)** — a Kubernetes apiserver feature where the client sends a partial object and the server reconciles per-field ownership. kube-vnet uses SSA with `FieldOwner("kube-vnet")` and `client.ForceOwnership` for all generated `NetworkPolicy` writes. See [ADR 0009](../adr/0009-server-side-apply-with-field-manager.md).

**System label** — an operator-stamped label of the form `kube-vnet.system/net.<homeNS>.<vnet>=<direction>` (`kube-vnet.system/net.cluster` for the cluster vnet; canonical per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)). Written by the `ResolutionReconciler` (or the *admission webhook*) based on the inheritance lattice (cluster baseline → namespace baseline → pod tier where bindings and pod labels intersect). Read by the policy generator's selectors. Protected from user mutation by a ValidatingAdmissionPolicy, or by the validating webhook when that is enabled. See [ADR 0031](../adr/0031-baseline-tier-resolution.md) and [ADR 0033](../adr/0033-canonical-fq-system-labels.md).

**System vnet** — an operator-managed `VirtualNetwork` resource labeled `kube-vnet.system/managed-by=kube-vnet`. Two of them: `namespace` (one per managed namespace) and `cluster` (one in the operator's namespace, with `allowedNamespaces.All=true`). Drift-corrected by the `SystemVnetReconciler`; protected from user mutation by a ValidatingAdmissionPolicy. See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md).

**`SystemVnetReconciler`** — the controller-runtime reconciler in `internal/controller/system_vnet_controller.go` that ensures the operator-managed `namespace` and `cluster` *system vnets* exist in every managed namespace and the operator namespace, respectively. Drift-corrects deletes. See [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md).

**`VirtualNetwork`** — the kube-vnet CRD. A named, namespaced resource. Pods join it by adding a label (or via a `VirtualNetworkBinding`); same-network pods can talk to each other in the directions their labels declare.

**`VirtualNetworkBaseline`** — a namespace-scoped singleton CRD (short names `vnbl`, `vnbls`) introduced in [ADR 0031](../adr/0031-baseline-tier-resolution.md). Named `default` per namespace (CEL-pinned). Carries namespace-wide tier-default memberships; can override `default-*` cluster-baseline entries.

**`VirtualNetworkBinding`** — a namespaced CRD (short names `vnb`, `vnbs`) that selects pods *in its own namespace* via a `podSelector` and attaches them to a target `VirtualNetwork` for a chosen `direction`. The escape hatch for enrolling pods whose template you can't modify (third-party Helm charts, other operators). `spec.virtualNetworkRef.{name,namespace}` names the target; the target vnet's `spec.allowedNamespaces` is enforced. Status: `Ready` condition with reasons `PodsAttached`, `NoPodsMatch`, `VirtualNetworkNotFound`, `NamespaceNotAllowed`, `NamespaceExcluded`, `UnknownDirection`, `InvalidSelector`; plus `attachedPods` and `observedGeneration`. The resolution controller stamps the canonical FQ system label `kube-vnet.system/net.<homeNS>.<vnet>` on selected pods; binding-targeted pods are covered by the regular per-`(vnet, namespace)` membership policy — no per-binding policy is emitted (per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)). See [ADR 0026](../adr/0026-virtualnetworkbinding-crd.md).

**`VirtualNetworkReconciler`** — the reconciler in `internal/controller/virtualnetwork_controller.go` that generates the per-vnet membership policies and vnet status. Does **not** own the baseline (that's the `NamespaceReconciler`).

**vnet** — short for `VirtualNetwork`. Also the CRD's short name (`kubectl get vnet`).
