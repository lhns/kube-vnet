# FAQ

Short answers to questions that come up before, during, or shortly after install.

For deeper "how it works", see [`concepts.md`](concepts.md). For "how do I do X", see [`recipes.md`](recipes.md).

---

## General

### Is kube-vnet production-ready?

The CRD is `v1alpha1`. Functionally, the operator handles the v1 design's acceptance criteria (CRD, reconciler, baseline, drift correction, cross-namespace cleanup, conditions/events/metrics, two-CNI e2e, signed/SBOM'd releases). Breaking changes are explicitly allowed across alpha versions — pin a specific version (`v0.X.Y`, not `latest`).

If you're running it in production, do all of:

- Pin a specific `image.tag` and Helm `--version`.
- Verify cosign signatures (see [`install.md`](install.md#verifying-signatures)).
- Watch the alerts in [`operations.md`](operations.md).

### What CNIs does it work with?

Anything that enforces standard `networking.k8s.io/v1` `NetworkPolicy`. Tested in CI against **kube-router** and **Calico**. Should also work fine with **Cilium**, **Antrea**, and recent **kindnetd**. EKS / GKE / AKS work as long as their CNI is configured to enforce NetworkPolicy.

It does **not** work with CNIs that ignore NetworkPolicy (older AWS VPC CNI without the policy add-on, plain Flannel, etc.).

### Is this a CNI?

No. kube-vnet is an operator that emits standard `NetworkPolicy` resources. Your CNI is what actually drops packets. We don't replace or wrap the CNI.

### Why is the CRD `v1alpha1`? When does `v1` happen?

When the open items in [ADR 0014](adr/0014-deferred-v1-items.md) are closed and the API has been stable through some real production usage. There's no fixed timeline.

### Why not just use NetworkPolicy directly?

You can. NetworkPolicy works. The reasons people reach for kube-vnet:

- The mental model fits how teams reason about connectivity (membership, not exception-based).
- The default-deny baseline is automatic and doesn't have to be re-remembered per namespace.
- Reviewing "is service X on the payments network?" is one label check, not a tour of label selectors.

If your cluster is small and you're comfortable hand-writing NetworkPolicy, kube-vnet is unnecessary overhead.

### How does this compare to Cilium L7 / NetworkPolicyV2 / AdminNetworkPolicy?

- **Cilium L7 policy** — operates at HTTP method / hostname level. kube-vnet is L3/L4 only. They compose: nothing in kube-vnet conflicts with a Cilium L7 policy in the same namespace.
- **AdminNetworkPolicy (ANP)** — the Kubernetes-native cluster-scoped policy with higher precedence than `NetworkPolicy`. Solves the namespace-RBAC-resistance problem that kube-vnet's drift-correction approximates with reconciliation. Tracked as the future direction in [ADR 0019](adr/0019-baseline-durability.md).

### Why is this `lhns.de`, not `kubernetes-sigs` / a Foundation project?

Personal project under [@lhns](https://github.com/lhns). The CRD group is `kube-vnet.lhns.de` because Kubernetes requires CRD groups to be domain-style.

---

## Architecture / API

### Why is the CRD namespaced and not cluster-scoped?

A VirtualNetwork always belongs to one application namespace that owns it. Reach (cross-namespace permissions) is a property of the network, not its identity — handled by `spec.allowedNamespaces` instead of by the CRD scope. See [ADR 0005](adr/0005-namespaced-crd-with-allowed-namespaces.md).

### Can pods join multiple VirtualNetworks?

Yes, additively. A pod with labels for vnets A and B can reach members of A *or* B (in the directions each label declares). Set the labels independently:

```yaml
labels:
  kube-vnet/net.payments: both
  kube-vnet/net.monitoring: egress
```

See [the bridge-pod recipe](recipes.md#bridge-pod-joining-two-vnets-sidecar--proxy-pattern).

### What are direction modes?

The join label *value* declares which directions the pod participates in: `both` (default, bidirectional), `ingress` (accept-only), `egress` (initiate-only), `none` (not a member). The legacy `"true"`/`"false"`/empty-string aliases were dropped per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md). Useful for asymmetric workloads — a logging sidecar uses `egress`, a read-only API uses `ingress`. See [`concepts.md`](concepts.md#direction-modes-on-the-join-label) and [ADR 0021](adr/0021-direction-modes-on-join-labels.md).

### Can I attach pods to a vnet without modifying their template?

Yes — use a `VirtualNetworkBinding` (short names `vnb`, `vnbs`). It selects pods *in its own namespace* via a `podSelector` and attaches them to a target vnet. Useful for third-party Helm charts or pods owned by another operator. Bindings live next to the pods they select; there's no cross-namespace binding. See [the binding recipe](recipes.md#enrolling-third-party-pods-via-virtualnetworkbinding) and [ADR 0026](adr/0026-virtualnetworkbinding-crd.md).

### Why one label per vnet, not a comma-separated list?

Three reasons (full discussion in [ADR 0003](adr/0003-one-label-per-virtualnetwork.md)):

1. The generated `NetworkPolicy` selector becomes trivial — `Exists` on a single key per network. No value enumeration.
2. Label values are capped at 63 characters; a comma-separated list of network names blows past that quickly.
3. It matches the standard Kubernetes "one label per category" convention.

### Does `allowedNamespaces` mean "any pod in those namespaces is allowed"?

**No.** `allowedNamespaces` controls which namespaces' pods are allowed to **join** the network. A pod in an allowed namespace still has to add the join label to become a member. Pods in those namespaces that don't carry the label get nothing.

This is the most common misconception about the API. See [`concepts.md` § Join eligibility, not blanket access](concepts.md#allowednamespaces-is-join-eligibility-not-blanket-access).

### Why is the CRD group `kube-vnet.lhns.de` instead of just `kube-vnet`?

Kubernetes requires CRD groups to be DNS-style (containing at least one dot). `kube-vnet` alone was rejected by the apiserver. The label key prefix (`kube-vnet/...`) is *not* subject to the same rule and stayed as it was.

### Do I need a validating admission webhook?

No. The CRD's CEL rule (introduced in Kubernetes 1.25) enforces name validation at admission. The reconciler does a defense-in-depth runtime check. See [ADR 0017](adr/0017-name-validation-via-cel-and-runtime-check.md).

---

## Operations

### What happens to my pods if the operator dies?

Existing `NetworkPolicy` resources stay in place. The apiserver continues serving them; the CNI continues enforcing them. Your data plane is unaffected.

What pauses: change propagation. New vnets aren't reconciled, label changes don't propagate, drift correction doesn't fire. Everything resumes when the operator comes back. See [`operations.md` § When the operator is down](operations.md#when-the-operator-is-down).

### How do I run kube-vnet in HA?

Set `replicaCount: 2` in the Helm values. Add anti-affinity so the replicas land on different nodes. Leader election is already on by default. See [`operations.md` § HA: two replicas across nodes](operations.md#ha-two-replicas-across-nodes).

### Does it work on EKS / GKE / AKS?

Yes if their CNI enforces NetworkPolicy:

- **EKS** — Calico, Cilium, or AWS VPC CNI with `enableNetworkPolicy=true`.
- **GKE** — Dataplane V2 or "Network Policy" add-on enabled.
- **AKS** — Azure CNI Powered by Cilium, Calico, or Azure NPM.

If you can `kubectl apply` a `NetworkPolicy` and have it actually enforce, kube-vnet works.

### Does kube-vnet need any special privileges I should be aware of?

It needs cluster-wide read on Pods and Namespaces, cluster-wide CRUD on `NetworkPolicy`, and CRUD on its own CRD's status subresource. Full inventory in [`security.md`](security.md#rbac-inventory).

### Can I install one operator per namespace?

Not really. The operator is designed to act cluster-wide because cross-namespace vnets need to install policies in foreign namespaces. Multiple operator instances would step on each other.

### Will the operator interfere with my existing NetworkPolicies?

No. It only owns objects labeled `kube-vnet.system/managed-by=kube-vnet`. Your policies are left alone. NetworkPolicies are additive in Kubernetes — your allows compose with the operator's. See [the coexistence recipe](recipes.md#coexisting-with-user-managed-networkpolicy).

### What about egress to the public internet?

kube-vnet does not restrict egress. The baseline carries `policyTypes: [Ingress]` only; egress (DNS, the apiserver, the public internet, other namespaces) is not blocked by the operator. Membership policies still grant egress allows to vnet peers, but generic egress is unrestricted. If you need per-workload egress restriction, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` — see [the per-workload egress allowlist recipe](recipes.md#per-workload-egress-allowlist-via-user-managed-networkpolicy). For threat-model implications see [`security.md`](security.md). The rationale is in [ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md).

### Why did egress just start working after the upgrade?

Behavior change with the `ingress-isolation` rename. The previous baseline blocked egress to anything that wasn't DNS or a vnet peer; the new baseline is `policyTypes: [Ingress]` only. Existing installs see their egress posture loosen on upgrade. If you relied on the previous egress restriction, write a user-managed `NetworkPolicy` per workload (the per-workload allowlist is also strictly more useful — the previous baseline's egress restriction was too coarse to actually contain the destinations that mattered). See [ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md).

---

## Security

### Can a namespace owner just delete the deny baseline?

Yes — if they have `delete networkpolicy` RBAC in their namespace, they can remove the `kube-vnet.base` baseline. The operator restores it within seconds (drift correction) and emits a `Warning PolicyRestored` Event for visibility. There's a small window between deletion and restore where the policy is gone.

For a hard guarantee, the proper Kubernetes tool is `AdminNetworkPolicy` (cluster-scoped, separate RBAC, higher precedence). Tracked as the future direction in [ADR 0019](adr/0019-baseline-durability.md). For now: monitor the `PolicyRestored` events; alert on repeated occurrences.

### How do I verify the image / chart I downloaded is genuine?

Cosign keyless. The signing identity is the release workflow itself. See [`install.md` § Verifying signatures](install.md#verifying-signatures).

### Is there an SBOM?

Yes. SPDX-JSON, attached as a Cosign attestation and as a plain release asset. See [`install.md` § Verifying SBOMs](install.md#verifying-sboms).

### How do you handle CVEs in dependencies?

Trivy runs on every PR (filesystem + image scans, CRITICAL/HIGH gates the build). Dependabot opens weekly bump PRs for Go modules, GitHub Actions, and the Dockerfile base image. See [`security.md`](security.md).

---

## Troubleshooting

### My pod has the join label but isn't a member — what gives?

Most common cause: wrong label form (bare vs prefixed), or the pod's namespace is excluded. See [`troubleshooting.md`](troubleshooting.md#my-pod-has-the-join-label-but-isnt-a-member).

### Pods I expect to be isolated can talk to each other — what gives?

Most common cause: your CNI doesn't enforce NetworkPolicy. See [`troubleshooting.md`](troubleshooting.md#pods-i-expect-to-be-isolated-can-talk-to-each-other).

### Why am I seeing "object has been modified" errors in the logs?

Benign. Optimistic-concurrency retries; the controller converges. See [`troubleshooting.md`](troubleshooting.md#operator-logs-are-noisy-with-conflict--object-has-been-modified-errors).

### Why am I seeing "PolicyRestored" warnings?

Someone (or something) deleted an operator-managed policy and the operator restored it. Investigate the source. See [`troubleshooting.md`](troubleshooting.md#i-see-policyrestored-warning-events--is-something-wrong).

---

## Comparison

### vs. raw `NetworkPolicy`

kube-vnet *generates* `NetworkPolicy`. You can always go back to writing it by hand — the policies kube-vnet generated keep working. Choose kube-vnet if the membership model fits your team's mental model better than label-selectors-and-exceptions.

### vs. NetworkPolicy V2 / AdminNetworkPolicy

Future direction. ANP solves the deny-baseline-durability problem more cleanly than reconciliation. When ANP is universally supported across CNIs and at v1, kube-vnet's baseline migrates to a cluster-scoped ANP; per-vnet allows stay as `NetworkPolicy`. Existing `VirtualNetwork` API surface doesn't change. See [ADR 0019](adr/0019-baseline-durability.md).

### vs. Cilium ClusterMesh / multi-cluster

Out of scope for v1. The original design doc anticipates a `Fleet` extent for cross-cluster networks; the v1 schema rejects `Fleet` so a v2 can extend cleanly. See the design doc's Future Improvements section.

### vs. service mesh (Istio, Linkerd)

Different layer. Service meshes do mTLS, identity, L7 routing, observability. kube-vnet does L3/L4 isolation via standard NetworkPolicy. They compose; nothing in kube-vnet conflicts with a service mesh.
