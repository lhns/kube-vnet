# kube-vnet Design Document

> **Reference status.** This is the original design document. Where it disagrees with the current implementation (notably: baseline is now ingress-only and decoupled from membership; the join label value carries a *direction*; long-form labels are accepted in the home namespace; `VirtualNetworkBinding` is the no-label alternative), the [ADRs](adr/README.md) are authoritative. See in particular ADRs 0021–0026.

`kube-vnet` is a Kubernetes operator that introduces a `VirtualNetwork` custom resource, giving the cluster a Docker-Swarm-style named-network primitive. The operator translates VirtualNetwork membership into standard `NetworkPolicy` resources.

## Problem Statement

Kubernetes' built-in `NetworkPolicy` resource is at the wrong level of abstraction for how operators and developers naturally think about service connectivity. Its model is *exception-based* (everything connects unless denied) and *selector-based* (connectivity expressed in terms of pod label selectors), whereas the natural mental model is *membership-based* (services join named networks) and *allowlist-by-construction* (only same-network pods communicate). Docker Swarm's network model is the canonical example of the better mental model.

This operator introduces a `VirtualNetwork` custom resource that gives Kubernetes a Swarm-like named-network primitive. Users define virtual networks; pods declare membership; the operator generates the underlying `NetworkPolicy` resources to enforce the connectivity. The output is standard `NetworkPolicy` — no CNI-specific extensions are required, and the abstraction can be removed cleanly by retaining the generated policies.

The "virtual" qualifier is deliberate: a `VirtualNetwork` is a logical grouping, not an actual network plane. Pods on the same `VirtualNetwork` continue to traverse the cluster's CNI as normal; the operator simply ensures that NetworkPolicies allow communication among same-VirtualNetwork members and (with the default-deny baseline) deny everything else.

## Goals

- Provide a `VirtualNetwork` CRD that models named virtual networks as first-class objects.
- Support `Namespace` and `Cluster` extent (whether the virtual network is restricted to one namespace or spans the cluster).
- Support a clear membership model where pods declare which virtual networks they join.
- Generate standard `NetworkPolicy` resources as the only enforcement mechanism. No CNI-specific resources in v1.
- Establish a default-deny baseline in any namespace where VirtualNetworks are used, so the abstraction is meaningful (not decorative).
- Be operable by a single engineer: small surface area, good status reporting, predictable behavior.

## Non-Goals (v1)

- **Cross-cluster (Fleet) extent.** Designed for in the schema for forward compatibility (see Future Improvements) but not implemented in v1. v1 is single-cluster only.
- **CNI-specific output** (CiliumNetworkPolicy, Calico GlobalNetworkPolicy, etc.). v1 produces standard `networking.k8s.io/v1` `NetworkPolicy` only. CNI-specific backends can be added later.
- **L7 / DNS / mTLS-identity policy.** These require either a service mesh or a CNI extension; out of scope for v1. v1 is L3/L4 only, matching what stock `NetworkPolicy` supports.
- **Egress to external (non-cluster) services as part of a VirtualNetwork.** VirtualNetworks model east-west connectivity between pods. External egress (allowing pods to reach the internet, external databases, etc.) should be handled by a separate primitive in a future iteration. For now, users add their own `NetworkPolicy` for external egress alongside VirtualNetworks.
- **Authorization for who may join a VirtualNetwork.** v1 trusts pod-declared membership. Multi-tenant scenarios where the VirtualNetwork owner needs to restrict joiners are deferred (see Future Improvements).
- **Pod-level intra-pod policy.** Containers within a pod share a network namespace; not policy-able by Kubernetes.

## Conceptual Model

### VirtualNetworks

A `VirtualNetwork` is a named, first-class object that pods can join. Pods on the same VirtualNetwork can communicate with each other. Pods *not* on a VirtualNetwork cannot reach pods *on* a VirtualNetwork (assuming default-deny baseline is active for that namespace).

A pod may be a member of multiple VirtualNetworks. Membership in VirtualNetwork A and VirtualNetwork B means the pod can reach any other pod in A *or* B; the VirtualNetworks compose additively at the pod level.

### Extent

Each VirtualNetwork has an `extent` field that describes the maximum reach of the VirtualNetwork:

- `extent: Namespace` (default) — the VirtualNetwork is local to the namespace it lives in. Only pods in the same namespace may join. Generated `NetworkPolicy` lives in that namespace and references only same-namespace pods.
- `extent: Cluster` — the VirtualNetwork is referenceable from any namespace in the cluster. Pods in any namespace may join. Generated `NetworkPolicy` resources are produced in each namespace that has joining pods, and they reference cross-namespace peers via `namespaceSelector`.

The extent is set on the VirtualNetwork itself, not on individual joins. A Namespace-extent VirtualNetwork simply cannot be joined from outside its namespace; the operator rejects (or ignores with a status condition) such joins.

### Membership

Pods declare VirtualNetwork membership through one label per VirtualNetwork they join. The label key encodes the VirtualNetwork name (and optionally a foreign namespace); the value is conventionally `"true"` but the operator only checks for key presence, not value content.

For VirtualNetworks in the pod's own namespace:

```
kube-vnet/net.<vnet-name>: "true"
```

For VirtualNetworks in a different namespace (only honored if `extent: Cluster`):

```
kube-vnet/net.<namespace>.<vnet-name>: "true"
```

The dot separator distinguishes the two forms: a single dot after `net.` means "VirtualNetwork in this pod's namespace"; two dots mean "namespace-prefixed reference." VirtualNetwork names cannot contain dots (the operator rejects such names with a validation error), so this encoding is unambiguous.

> **Why one label per VirtualNetwork rather than a comma-separated list?** Three reasons. First, generated `NetworkPolicy` selectors become trivial — `matchExpressions` with `operator: Exists` on the relevant key, no enumeration of value combinations needed. Second, label values have a 63-character limit, which a comma-separated list of network names blows past quickly; one label per network has no aggregate ceiling. Third, this matches Kubernetes label conventions ("this resource is in category X" is one label per category, not a delimited string in one value).

The operator's selector for "pods that are members of VirtualNetwork `payments` in namespace `platform`" is simply:

```yaml
matchExpressions:
  - key: kube-vnet/net.payments
    operator: Exists
```

For a Cluster-extent VirtualNetwork referenced from a foreign namespace, the operator looks for either form depending on which namespace it is generating policy in (the home namespace uses the bare form, foreign namespaces use the namespace-prefixed form).

Invalid references (missing VirtualNetwork, joining a Namespace-extent VirtualNetwork from outside its namespace, etc.) are surfaced as a condition on the *VirtualNetwork* status (under `Degraded` or similar) and logged. The pod itself is unaware of the rejection; it simply doesn't get connectivity to that VirtualNetwork.

> The label prefix is configurable via operator config (default `kube-vnet/`) so users can avoid collisions with other tooling.

### Default-Deny Baseline

For each namespace where at least one pod is joining at least one VirtualNetwork, the operator ensures a default-deny `NetworkPolicy` exists. Without this baseline, the VirtualNetwork abstraction is decorative — pods that aren't on any VirtualNetwork would still be reachable because Kubernetes' default is allow-all.

The operator manages this baseline itself, so users do not have to remember to create it. The baseline is a `NetworkPolicy` named `kube-vnet-default-deny` in the namespace, with empty pod selector and ingress/egress rules. Egress to `kube-system`/CoreDNS is allowed in the baseline (see DNS below).

The operator removes the baseline when no VirtualNetworks are in use in the namespace anymore (i.e., the operator is no longer responsible for the namespace).

> If the user has their own `NetworkPolicy` resources in the namespace, they continue to work; the operator's baseline composes additively (NetworkPolicies are ORed by Kubernetes). Users may also opt out of the baseline per-namespace via the namespace annotation `kube-vnet/baseline: disabled`, in which case they take responsibility for default-deny themselves.

### DNS Allowance

Every pod needs to reach CoreDNS or it cannot resolve any names. The default-deny baseline includes egress allowance to UDP/TCP 53 for the `kube-system` namespace's CoreDNS pods (selected by namespace label and by the standard `k8s-app: kube-dns` label).

This is non-negotiable in v1. The alternative — making users remember to add DNS rules to every VirtualNetwork — is a footgun the operator should close.

## CRD Schema

### VirtualNetwork (`kube-vnet/v1alpha1`)

```yaml
apiVersion: kube-vnet/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
spec:
  extent: Namespace      # Namespace (default) | Cluster
  description: |         # optional, for documentation
    VirtualNetwork connecting all payments-related services.
status:
  conditions:
    - type: Ready
      status: "True"
      reason: PoliciesGenerated
      message: "5 NetworkPolicies generated across 2 namespaces"
      lastTransitionTime: "..."
  members:                # observed pod members of this VirtualNetwork
    - namespace: platform
      pods:
        - orders-7c5f4b-abc12
        - orders-7c5f4b-def34
    - namespace: webapp
      pods:
        - frontend-9d8e7-xyz98
  generatedPolicies:      # references to NetworkPolicies the operator created
    - namespace: platform
      name: kube-vnet-payments-platform
    - namespace: webapp
      name: kube-vnet-payments-webapp
  observedGeneration: 3
```

**Field semantics:**

- `spec.extent` — see Extent above. Default `Namespace` if unset.
- `spec.description` — free text. Not interpreted by the operator; surfaced in status for documentation.
- `status.conditions` — standard Kubernetes condition pattern. `Ready` is the primary condition. Others may be added: `MembersDiscovered`, `PoliciesReconciled`, `Degraded`.
- `status.members` — observed pod membership, grouped by namespace. Updated on each reconciliation.
- `status.generatedPolicies` — list of `NetworkPolicy` resources the operator owns for this VirtualNetwork. Useful for debugging and for cleanup verification.
- `status.observedGeneration` — standard pattern; lets users tell whether status reflects the latest spec.

**CRD-level scope:** the `VirtualNetwork` CRD is itself namespaced (`scope: Namespaced` in the CRD definition). A Cluster-extent VirtualNetwork still lives in a specific namespace, which is its "home" — the namespace whose ownership is reflected in the resource. References from other namespaces use the `<namespace>/<name>` form.

**kubectl short names:** the CRD declares `vnet` and `vnets` as short names. `kubectl get vnet -A` lists all VirtualNetworks in the cluster.

### Pod Label Convention

Pods declare membership via one label per VirtualNetwork on the pod template. Bare references (`kube-vnet/net.<name>`) refer to VirtualNetworks in the pod's namespace; namespace-prefixed references (`kube-vnet/net.<namespace>.<name>`) refer to VirtualNetworks in other namespaces (only honored if the target has `extent: Cluster`).

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders
spec:
  template:
    metadata:
      labels:
        app: orders
        kube-vnet/net.payments: "true"
        kube-vnet/net.monitoring: "true"
        kube-vnet/net.kube-system.observability: "true"
```

- One label per VirtualNetwork joined.
- Value is conventionally `"true"` but the operator only checks key presence; any value works.
- Bare form `kube-vnet/net.<name>` for same-namespace references.
- Namespace-prefixed form `kube-vnet/net.<namespace>.<name>` for cross-namespace references.
- VirtualNetwork names must not contain dots (the operator validates this).
- Invalid references (missing VirtualNetwork, wrong extent, etc.) are surfaced as a condition on the *VirtualNetwork* status (under `Degraded` or similar) and logged. The pod itself is unaware of the rejection; it simply doesn't get connectivity to that VirtualNetwork.

> The label prefix is configurable via operator config (default `kube-vnet/`) so users can avoid collisions with other tooling.

## Generated NetworkPolicy

For each VirtualNetwork with members, the operator generates one `NetworkPolicy` per namespace that contains members of that VirtualNetwork. Each generated policy:

- Is named deterministically: `kube-vnet-<vnet-name>-<namespace>` (e.g., `kube-vnet-payments-webapp`). Truncated/hashed if too long (Kubernetes resource name limit is 253 characters).
- Has `metadata.labels[kube-vnet/managed-by] = "kube-vnet"` and `metadata.labels[kube-vnet/network] = "<namespace>.<name>"` so the operator can find them. (Dot rather than slash because `/` is not allowed in label values.)
- Has an `ownerReference` to the parent VirtualNetwork resource (cross-namespace owner references are not supported by Kubernetes, so for Cluster-extent VirtualNetworks, the operator manages ownership manually — it must clean up policies in foreign namespaces when the VirtualNetwork is deleted).
- Has a `podSelector` matching pods with the join label containing this VirtualNetwork.
- Has ingress rules allowing traffic from peers (other members of the same VirtualNetwork) in this namespace and (for Cluster-extent) in any namespace where members exist.
- Has egress rules symmetric to ingress, plus the standard CoreDNS allowance.

### Example: Namespace-extent VirtualNetwork with one member

VirtualNetwork `payments` in namespace `platform`, joined by pods labeled `kube-vnet/net.payments`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet-payments-platform
  namespace: platform
  labels:
    kube-vnet/managed-by: kube-vnet
    kube-vnet/network: platform.payments
  ownerReferences:
    - apiVersion: kube-vnet/v1alpha1
      kind: VirtualNetwork
      name: payments
      uid: ...
spec:
  podSelector:
    matchExpressions:
      - key: kube-vnet/net.payments
        operator: Exists
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector:
            matchExpressions:
              - key: kube-vnet/net.payments
                operator: Exists
  egress:
    - to:
        - podSelector:
            matchExpressions:
              - key: kube-vnet/net.payments
                operator: Exists
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
```

The selector is now trivial — a single `Exists` check on the per-VirtualNetwork label. No combination tracking, no value enumeration, no canonicalization concerns.

### Example: Cluster-extent VirtualNetwork spanning two namespaces

VirtualNetwork `observability` in namespace `kube-system` with `extent: Cluster`, joined by pods in `platform` and `webapp`:

The operator generates two `NetworkPolicy` resources, one in `platform` and one in `webapp`. In the home namespace (`kube-system`), members use the bare label `kube-vnet/net.observability`. In foreign namespaces (`platform`, `webapp`), members use the namespace-prefixed label `kube-vnet/net.kube-system.observability`. The generated policies in foreign namespaces match on the namespace-prefixed key; the policy in the home namespace (if any local members exist) matches on the bare key. Cross-namespace peer references use `namespaceSelector` plus `podSelector`, with the `podSelector` keyed on whichever form is appropriate for the peer's namespace.

## Reconciliation Logic

The operator runs a single reconciler that watches:

- `VirtualNetwork` (custom resource) — primary watch
- `Pod` — to track membership changes (with a label-selector-based filter for efficiency)
- `NetworkPolicy` — to detect drift on operator-managed policies

On each reconciliation of a VirtualNetwork:

1. **Compute desired members**: list pods (cluster-wide for Cluster-extent, namespace-local for Namespace-extent) whose join label includes this VirtualNetwork. Reject cross-namespace joiners for Namespace-extent VirtualNetworks; record those rejections in status conditions.
2. **Compute desired NetworkPolicies**: one per namespace with members. Build the policy spec as described above.
3. **Ensure default-deny baselines**: for each namespace with at least one operator-managed VirtualNetwork membership, ensure the baseline `NetworkPolicy` exists.
4. **Apply policies**: server-side apply the generated NetworkPolicies. Use field ownership to coexist cleanly with user-managed policies.
5. **Delete stale policies**: any operator-managed `NetworkPolicy` that is no longer needed (VirtualNetwork deleted, no members in namespace anymore, etc.) is deleted.
6. **Update status**: `members`, `generatedPolicies`, `conditions`, `observedGeneration`.

On Pod events: enqueue the VirtualNetworks the pod claims to join (from its label) plus the VirtualNetworks it was previously observed to join (from the operator's cache), so removals are handled.

On NetworkPolicy events for operator-managed policies: enqueue the owning VirtualNetwork. Drift correction.

### Reconciliation triggers

- VirtualNetwork create/update/delete
- Pod create/update/delete with the join label
- NetworkPolicy update/delete on operator-managed policies (drift detection)
- Periodic resync (every 10 minutes) as a safety net

### Idempotency and safety

- All operations on `NetworkPolicy` are idempotent (server-side apply with a stable field manager name `kube-vnet`).
- The operator never modifies user-managed `NetworkPolicy` resources. It identifies its own policies via the `kube-vnet/managed-by` label.
- Deletion of a VirtualNetwork triggers deletion of all owned `NetworkPolicy` resources, including those in foreign namespaces (Cluster-extent case). For Namespace-extent, owner references handle this; for Cluster-extent, the operator deletes manually since cross-namespace owner references aren't supported.

## Status and Observability

### Status Conditions

The VirtualNetwork's `status.conditions` use the standard Kubernetes condition pattern. Conditions to support:

- **Ready** — true if all desired policies have been successfully generated and applied.
- **Degraded** — true if any subset of joiners couldn't be honored (cross-namespace join to Namespace-extent VirtualNetwork, missing target VirtualNetwork, etc.). Includes details in the message.
- **Reconciling** — true while the operator is mid-reconciliation. Optional; useful for debugging.

### Logging

- One log line per VirtualNetwork reconciliation, structured (key=value), at info level: VirtualNetwork name, namespace, member count, policies generated, duration.
- Errors at error level with full context.
- Debug level for per-pod membership computation and per-policy diff.

### Metrics (Prometheus)

- `kube_vnet_reconciliations_total{result="success|error"}` — counter
- `kube_vnet_reconcile_duration_seconds` — histogram
- `kube_vnet_networks_total{extent="Namespace|Cluster"}` — gauge
- `kube_vnet_managed_policies_total` — gauge
- `kube_vnet_members_total{network="ns/name"}` — gauge

### Events

Emit Kubernetes Events on the VirtualNetwork resource for:

- First Ready transition
- Transition to/from Degraded
- Significant errors (failed to apply policies, etc.)

## Project Layout

The implementation should follow the standard `kubebuilder`/`controller-runtime` layout:

```
kube-vnet/
├── api/
│   └── v1alpha1/
│       ├── virtualnetwork_types.go # CRD Go types
│       ├── groupversion_info.go
│       └── zz_generated.deepcopy.go
├── cmd/
│   └── main.go                     # operator entrypoint
├── internal/
│   └── controller/
│       ├── virtualnetwork_controller.go   # main reconciler
│       ├── policy_generator.go            # NetworkPolicy generation logic
│       ├── baseline.go                    # default-deny baseline management
│       └── virtualnetwork_controller_test.go
├── config/
│   ├── crd/                        # CRD manifests
│   ├── rbac/                       # operator RBAC
│   ├── manager/                    # operator Deployment
│   └── samples/                    # example VirtualNetwork resources
├── test/
│   └── e2e/                        # envtest-based end-to-end tests
├── go.mod
├── Dockerfile
├── Makefile
└── README.md
```

## RBAC Requirements

The operator's ServiceAccount needs:

- `virtualnetworks.kube-vnet`: get, list, watch, update (status), patch (status)
- `networkpolicies.networking.k8s.io`: get, list, watch, create, update, patch, delete (cluster-wide, since Cluster-extent VirtualNetworks generate policies in foreign namespaces)
- `pods`: get, list, watch (cluster-wide)
- `namespaces`: get, list, watch (cluster-wide, for namespace selector matching)
- `events`: create, patch (in operator's own namespace and in the namespace of the VirtualNetwork being reconciled)
- Leader election (configmaps/leases) in the operator's own namespace

## Testing Strategy

### Unit tests

- Policy generation: given a VirtualNetwork spec and a set of pods, assert the generated `NetworkPolicy` is correct.
- Membership computation: given a list of pods with various join labels, assert membership is computed correctly, including invalid references.
- Baseline logic: given a namespace state, assert the baseline policy is created/retained/deleted as expected.

### Integration tests (envtest)

- Create a VirtualNetwork, create pods, assert NetworkPolicies are generated with the expected spec.
- Update VirtualNetwork extent from Namespace to Cluster, assert policies are regenerated correctly.
- Delete a VirtualNetwork, assert all owned policies are removed (including cross-namespace ones).
- Cross-namespace join to a Namespace-extent VirtualNetwork: assert the join is rejected and surfaced in status.

### End-to-end tests (kind cluster)

- A small set of scenarios that actually exercise traffic flow: deploy two pods on the same VirtualNetwork, assert connectivity. Deploy two pods on different VirtualNetworks, assert isolation. Requires a CNI that enforces NetworkPolicy (Calico, Cilium, etc.) in the test cluster.

## Future Improvements (out of scope for v1)

- **Fleet extent (cross-cluster).** The schema reserves the word `Fleet` as a future value of `extent`. Implementation requires a Cluster registry CRD, cross-cluster transport (e.g., WireGuard, with the operator generating peer configurations), identity propagation across clusters, and possibly cross-cluster DNS. This is a substantial v2 effort. The v1 schema validates `extent` as one of `[Namespace, Cluster]` only and rejects `Fleet` with a clear error so that a v2 upgrade can extend the enum without compatibility breaks.

- **CNI-specific backends.** A pluggable backend system that can generate `CiliumNetworkPolicy` (for L7 / DNS-based egress) or `Calico GlobalNetworkPolicy` (for tier ordering) instead of (or in addition to) standard `NetworkPolicy`. Requires a `backend` field in the operator config or per-VirtualNetwork spec.

- **External egress as a first-class concept.** A separate `ExternalService` or `EgressTarget` CRD that pods can reference, so external connectivity is also membership-based rather than IP-CIDR-based.

- **Authorization on join.** A `joinPolicy` field on the VirtualNetwork that restricts which pods/namespaces may join. Useful for multi-tenant scenarios; not needed for single-operator setups.

- **VirtualNetworkBinding CRD as alternative to label-based join.** For cases where cross-namespace joins should be reviewed/audited as their own resource rather than implicit via label.

- **Service-level (not just pod-level) network identity.** Currently membership is by pod. A service-level abstraction where a Service joins a VirtualNetwork and the policy applies to the Service's selector might be more ergonomic for some workflows.

- **Status detail on the joining workload.** Right now, a pod that fails to join a VirtualNetwork only surfaces that failure on the VirtualNetwork's status. Mirroring this onto the workload (via an annotation or event) would help workload owners debug.

## Open Questions for Discussion

1. **Policy naming collisions.** `kube-vnet-<vnet-name>-<namespace>` could collide with user-managed NetworkPolicies that happen to use the same prefix. Consider a hash suffix on top of the prefix, or rely on the `kube-vnet/managed-by` label as the source of truth and use a less collision-prone name. Decide before v1.

2. **Should the baseline default-deny be opt-in rather than automatic?** The case for automatic: the abstraction is meaningless without it. The case for opt-in: respects user-managed namespaces. v1 proposes automatic with a per-namespace opt-out annotation (`kube-vnet/baseline: disabled`). Confirm this is the right default.

3. **What does the operator do with namespaces that have VirtualNetworks but no joining pods?** Probably nothing — no policies needed, no baseline needed. Confirm.

4. **Should `extent: Cluster` VirtualNetworks have a "home namespace" semantically, or be globally addressable without one?** v1 keeps the home namespace because the VirtualNetwork resource itself has to live somewhere, and it gives a natural ownership/RBAC anchor. But the alternative (cluster-scoped `VirtualNetwork` CRD) is also defensible. Decide before v1.

5. **Label cardinality at large scale.** One-label-per-network means a pod on N VirtualNetworks has N labels. For typical deployments this is trivial, but a pod that joins dozens of VirtualNetworks could push label counts higher than usual. Worth a stress test before declaring v1 stable.

6. **Reserved VirtualNetwork name validation.** VirtualNetwork names cannot contain dots (because the label encoding uses dots as separators between namespace and name). The CRD's OpenAPI schema should enforce this with a regex pattern. Decide on the pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` (DNS-1123 label) is probably right.

## Implementation Suggestions

- Use `kubebuilder` to scaffold the project. The standard `controller-runtime` patterns handle most of what's needed.
- Use server-side apply (`client.Apply`) for `NetworkPolicy` management, with a stable field manager name `kube-vnet`. This handles coexistence with user-managed policies cleanly.
- Use a label selector predicate on the Pod watch to avoid reconciling on every pod in the cluster — only pods with the join label are interesting.
- The cross-namespace ownership cleanup (for Cluster-extent VirtualNetworks) is the trickiest part. Consider tracking owned policies in the VirtualNetwork's `status.generatedPolicies` and using that as the source of truth for cleanup, rather than relying on label-selecting all matching policies cluster-wide.
- Write the policy generator as a pure function (VirtualNetwork spec + pod list → NetworkPolicy specs). This makes it trivially unit-testable and keeps the controller logic thin.
- Defer all CNI-backend abstraction in v1. Hardcode standard `NetworkPolicy` output. The future-improvement section anticipates extending this; don't pre-abstract.

## Acceptance Criteria for v1

- A user can `kubectl apply` a VirtualNetwork resource and add the join label to a Deployment, and the appropriate `NetworkPolicy` resources are created.
- Two pods on the same VirtualNetwork can communicate; two pods on different (or no) VirtualNetworks cannot.
- Cluster-extent VirtualNetworks correctly span multiple namespaces.
- Default-deny baseline is established automatically in namespaces with VirtualNetwork membership.
- VirtualNetwork deletion removes all generated policies, including across namespaces.
- Status conditions accurately reflect the operator's view of the world.
- Drift on operator-managed policies is corrected within one reconciliation cycle.
- Operator runs cleanly with leader election in HA mode.
- All tests pass; e2e tests demonstrate actual traffic enforcement on a kind cluster with Calico or Cilium.
