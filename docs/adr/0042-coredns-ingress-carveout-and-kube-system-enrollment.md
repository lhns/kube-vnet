# ADR 0042 — CoreDNS ingress carve-out and kube-system enrollment

**Status**: Accepted (2026-07-09)

## Context

kube-vnet has shipped `disabledNamespaces = [kube-system, kube-public, kube-node-lease]` since ADR 0029: the operator stays out of those namespaces entirely, so their pods are allow-all. Users increasingly want to enroll `kube-system` — to segment its workloads, or simply to stop carving a hole in an otherwise-managed cluster.

The moment `kube-system` is removed from the disabled list, the operator's deny-all baseline applies to every pod there. Almost everything survives:

- **hostNetwork pods** (kube-proxy, konnectivity, kube-router, CNI agents, nllb) — kube-vnet skips them; NetworkPolicy generally isn't enforced on host-network pods anyway.
- **metrics-server** — reached by the apiserver via its aggregated `APIService`, already covered by the `ext.apiserver` auto-allow family (ADR 0041).

The one casualty is **CoreDNS**. Its inbound `:53` is a plain ClusterIP Service (`kube-dns`), matched by none of the auto-allow families (`ext.svc` gates on LoadBalancer/NodePort/externalIP exposure; `ext.host` on hostPort; `ext.apiserver` on webhook/APIService references). So CoreDNS goes deny-all and **cluster DNS dies cluster-wide** — every pod loses name resolution. This is a catastrophic, easy-to-hit footgun: "removed kube-system from the disabled list, whole cluster's DNS broke."

DNS is not a segmentation problem. Every pod queries `:53` regardless of which vnet (if any) it belongs to, and hostNetwork clients query it from the **node IP**. It is *universal reachability* — reachable by everything, member or not, pod or node.

## Decision

Two changes:

### 1. Ship a chart-templated CoreDNS ingress carve-out

`charts/kube-vnet/templates/coredns-dns-allow.yaml` renders a NetworkPolicy that re-opens CoreDNS `:53` from `0.0.0.0/0`, additive to the operator's deny-all baseline (NetworkPolicies union):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet-coredns-allow
  namespace: {{ .Values.dnsCarveout.namespace }}     # default kube-system
  labels:
    # chart labels ONLY (app.kubernetes.io/*, helm.sh/chart) — NOT kube-vnet.system/*
spec:
  podSelector:
    matchLabels: {{ .Values.dnsCarveout.selector }}   # default {k8s-app: kube-dns}
  policyTypes: [Ingress]
  ingress:
    - from: [{ ipBlock: { cidr: 0.0.0.0/0 } }]
      ports: {{ .Values.dnsCarveout.ports }}          # default 53/UDP + 53/TCP
```

Design points:

1. **Chart-shipped, not operator-generated.** It carries only the standard chart labels, never `kube-vnet.system/*`, so the operator's baseline/membership/external sweeps (which filter on `kube-vnet.system/managed-by=kube-vnet` + a role label) never see or delete it. Its lifecycle is the Helm release's — the correct home for a single, static, cluster-infra carve-out.
2. **Auto-rendered when the DNS namespace is managed.** The template renders iff `dnsCarveout.namespace` is *not* in `operator.disabledNamespaces` (with `dnsCarveout.enabled: true|false` as an explicit force/suppress override). Because `disabledNamespaces` is a Helm value, enrolling kube-system is itself a `helm upgrade` — so the carve-out ships in the *same apply* that removes the deny-all exemption. The footgun is closed by construction: you cannot enroll kube-system via the chart without the DNS carve-out coming with it.
3. **`ipBlock: 0.0.0.0/0`, port-scoped.** The same shape the `ext.*` families already emit for "reachable from everywhere." `0.0.0.0/0` (not `namespaceSelector: {}`) because DNS clients include hostNetwork pods and node-level resolvers whose source is a node IP, never an in-cluster pod IP. Scoped to `:53` (values-driven) so only DNS is opened, not CoreDNS's other ports.

### 2. Narrow the default `disabledNamespaces` to `[kube-system]`

`kube-public` and `kube-node-lease` hold no pods, so managing them is inert (a deny-all baseline matches zero pods; the system `namespace` vnet created in each does nothing). They are dropped from the default. `kube-system` stays disabled by default — it is the only one with cluster-critical pods where deny-all bites — and enrolling it is the supported, DNS-safe opt-in above. Changed in both `cmd/main.go` (`--disabled-namespaces` default) and `charts/kube-vnet/values.yaml` (`operator.disabledNamespaces`).

## Alternatives considered

1. **VirtualNetworkBinding / bind CoreDNS to the cluster vnet.** Semantically wrong for DNS. A membership policy allows ingress only from *co-members* of the vnet; `allowedNamespaces.all` governs who may *join*, not who may *reach*. Under pod/namespace isolation, a pod not in the cluster vnet (most pods) hits CoreDNS's membership policy, doesn't match, and loses DNS. It only works under cluster isolation, where every pod already has default cluster-vnet membership. It also can never allow hostNetwork/node clients (a node IP is never a vnet member).
2. **Dedicated `dns` vnet + all-namespaces binding.** Fails the same way and adds cost: the vnet/binding model has no `ipBlock` field, so a binding compiles to `namespaceSelector: {}` = "all *in-cluster pods*," strictly narrower than `0.0.0.0/0` — still denies hostNetwork/node clients. And making all pods reach CoreDNS requires binding *every pod in the cluster* into the dns vnet, at `direction=egress` exactly (any `both` makes all pods co-members → a cluster-wide allow-all mesh that erases isolation), forcing the resolution controller to stamp membership on and reconcile every pod forever.
3. **`ext.dns` operator auto-allow family.** A 4th controller that detects the DNS Service and emits the same `0.0.0.0/0` NP. Correct, but overkill: the other three families are operator code because they track *dynamic, many, distro-varying* targets with named-port resolution. Cluster DNS is a single, static, well-known Service with numeric targetPort 53 — auto-discovery earns little against the cost of a controller + ADR + three test tiers.
4. **A general `allowedCIDRs` external-peer on VirtualNetwork.** Add `spec.allowedCIDRs` (with per-CIDR ports) as a sibling of `allowedNamespaces`, then model DNS as a dns-vnet using it. This is the only vnet-native way to reach true `0.0.0.0/0`, and a legitimate feature *if external-CIDR exposure is wanted for its own sake* (external monitoring, off-cluster ingress). But vnets are all-ports today, so a safe version needs per-peer port-scoping — a real model extension not justified by DNS alone. Parked as possible future work.

## Consequences

- **Positive**: enrolling `kube-system` is safe out of the box — remove it from `disabledNamespaces` and cluster DNS keeps working with no extra user action.
- **Positive**: the DNS carve-out is transparent (a visible NetworkPolicy the user can inspect/override) and fully values-driven (`namespace`/`selector`/`ports`), so nonstandard DNS (different label, port, or namespace) is a values edit, not a code change.
- **Positive**: no new operator code, no new controller, no new admission surface. ~50 lines of chart template.
- **Positive**: shorter default disabled list; the two podless namespaces are no longer special-cased.
- **Negative**: the carve-out is a NetworkPolicy that lives *outside* the operator's ownership model — it won't self-heal if a user deletes it (Helm re-applies on the next upgrade, not continuously). Acceptable for a static release-scoped object.
- **Negative**: `0.0.0.0/0` on `:53` is permissive, but it matches the pre-kube-vnet default (any pod could always reach CoreDNS) and DNS has no authentication layer NetworkPolicy would be protecting.

## References

- [ADR 0029 — Allow-all baseline in mode=none, and system namespaces disabled by default](0029-allow-all-baseline-and-system-ns-disabled.md) (origin of the disabled-by-default set; this ADR narrows it to `[kube-system]`)
- [ADR 0030 — Unified vnet-membership model with resolution layer](0030-unified-vnet-membership-with-resolution.md) (carried the disabled-namespaces decision forward)
- [ADR 0038 — Auto-allow externally-exposed Services](0038-auto-allow-externally-exposed-services.md) (the `ipBlock: 0.0.0.0/0` auto-allow pattern this reuses)
- [ADR 0041 — Auto-allow Services reached by the apiserver](0041-auto-allow-apiserver-reachable-services.md) (covers metrics-server; the other kube-system reachability gap)
