# ADR 0038 — Auto-allow externally-exposed Services

> **Note (ADR 0039 amendment, 2026-06-26)**: the policy name shape `kube-vnet.external-<svcName>-<8hex>` referenced in this ADR is now `kube-vnet.ext.svc.<svcName>-<8hex>`. The `kube-vnet.system/source` label value changed from `service/<svcName>` to a bare `<svcName>` in 2c798f2 (label values can't contain `/`). See [ADR 0039](0039-uniform-kind-prefixed-policy-naming.md) for the new naming convention.

**Status**: Accepted (2026-06-26)

## Context

NetworkPolicy maps poorly to the "Service of type LoadBalancer / NodePort" pattern that every ingress controller, every LB-fronted admin UI, every public-facing dashboard relies on. Once *any* `Ingress` NetworkPolicy selects a pod, that pod is default-deny for every source IP not named in a `from:` rule. `namespaceSelector` only matches **pod-IP** sources; external traffic — coming in via the LoadBalancer's external IP (kube-proxy SNATs to the node IP in `externalTrafficPolicy: Cluster` mode), via a NodePort + node IP, or via an external client whose IP is preserved (`externalTrafficPolicy: Local`) — never lives in any K8s namespace and therefore never matches any `namespaceSelector`.

Concrete user symptom this ADR closes:

> traefik runs as a DaemonSet on every node, with `Service: LoadBalancer` on port 80, in the `traefik` namespace. After installing kube-vnet (any of the three baseline presets), traefik is unreachable from outside the cluster: the baseline + per-vnet membership policies select traefik's pods, and the `from:` rules only mention pod namespaces. External LB-routed packets — SNAT'd to node IPs — match nothing. Traefik 200s from inside the cluster, 504s from outside.

Existing escape hatches before this ADR:

- `kube-vnet/disabled=true` on the namespace — turns kube-vnet off entirely for that NS. Loses pod-to-pod isolation.
- Hand-written `NetworkPolicy` with `ipBlock: 0.0.0.0/0` — works but gets stale; if the Service's `targetPort` changes, the hand-written allow doesn't follow.

Both lose information the operator already has: the apiserver knows which Services are externally exposed and what ports they expose. The operator can emit exactly the right NetworkPolicy without the user having to write it.

## Decision

A new `ExternalAllowReconciler` watches `corev1.Service` and emits a dedicated **external-allow `NetworkPolicy`** per externally-exposed Service. The emitted policy is additive — it shares the namespace with kube-vnet's baseline + membership policies and composes via NetworkPolicy union semantics: pod-to-pod isolation via vnet membership keeps working unchanged, *and* external traffic on the exposed `targetPort` reaches the pod.

### What triggers an emission

| Source | Detected via |
|---|---|
| `Service` of `type: LoadBalancer` | watch `corev1.Service` |
| `Service` of `type: NodePort` | watch `corev1.Service` |
| `Service` of `type: ClusterIP` with non-empty `spec.externalIPs` | watch `corev1.Service` |

Headless Services (`clusterIP: None`), `ExternalName` Services, and Services with no `spec.selector` (manually-managed Endpoints) **don't** trigger emission — there's no pod set to scope the policy to.

### The emitted policy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.ext.svc.<service-name>-<8hex>
  namespace: <service-NS>
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/role: external-allow
    kube-vnet.system/source: service/<service-name>
  ownerReferences:
    - kind: Service        # apiserver GC cascades on Service delete
      name: <service-name>
      controller: true
      blockOwnerDeletion: true
spec:
  podSelector:
    matchLabels: { ...verbatim from Service.spec.selector }
  ingress:
    - from:
        - ipBlock: { cidr: 0.0.0.0/0 }
      ports:
        - protocol: TCP   # whatever Service declares; defaults to TCP
          port: <Service.spec.ports[*].targetPort>
  policyTypes: [Ingress]
```

Three design points worth calling out:

1. **`from: ipBlock 0.0.0.0/0`** — matches both externally-SNAT'd node IPs (`externalTrafficPolicy: Cluster`) and original-client IPs (`Local`). The CIDR isn't narrower because we have no portable way to know the LB's true source CIDR; users who want narrower scoping use the opt-out and write their own NetworkPolicy.
2. **Port-scoped, always to `targetPort`.** kube-proxy DNATs `node:nodePort` → `pod:targetPort` before the packet ever hits the pod. NetworkPolicy on the pod sees `targetPort`. Whitelisting `nodePort` would whitelist the wrong port and miss everything. Admin/metrics ports on the same pod stay protected because they don't appear in the Service's `ports[]`.
3. **Owner reference on the policy → Service.** Same-NS cascade-delete works via apiserver GC, no reconciler needed when the Service goes away.

### Opt-out

Single annotation, default-on:

```bash
# Per Service:
kubectl annotate -n traefik svc traefik kube-vnet/external-allow=false

# Per namespace:
kubectl annotate ns traefik kube-vnet/external-allow=false
```

Only the literal value `"false"` opts out — `"true"`, empty, `"FALSE"`, `"no"`, anything else, leaves auto-emit on. Asymmetric on purpose: this is a "user explicitly said no" signal, not a generic boolean.

Removing the annotation re-enables auto-emit on the next reconcile.

### Why we don't watch `Ingress` or Gateway API

Asked early in design: shouldn't we follow `Ingress` rules to backend Services, or watch `Gateway` / `HTTPRoute`?

No — the ingress controller (traefik / nginx-ingress / the Gateway data plane pods) is **what physically receives** external traffic. That controller is itself fronted by a `Service: LoadBalancer` — which row 1 of the trigger table covers. Backend Services chained off `Ingress.spec.rules` or `HTTPRoute.spec.parentRefs` are reached **from the ingress controller pods**, which is plain pod-to-pod traffic governed by normal vnet membership rules. There's no traffic shape that requires Gateway/Ingress awareness; watching those resources would be wasted complexity.

## Consequences

- Ingress controllers and LB-backed admin UIs are reachable from outside the cluster by default in kube-vnet-managed namespaces. The traefik scenario above just works after install — no annotation, no hand-written policy.
- Pod-to-pod isolation is preserved. The emitted policy is port-scoped (only the Service's exposed `targetPort`s) and additive (other policies' selectors and from-rules still apply). A pod that exposes :80 via LB and runs admin tools on :9100 keeps the :9100 protection it already had.
- The chart's `system-labels-vap` (extended in ADR 0037) already protects any policy carrying a `kube-vnet.system/*` label. External-allow policies inherit that protection — users can't create, delete, or mutate them outside the operator.
- The pre-delete cleanup hook (ADR 0036) cleans up external-allow policies automatically: it selects on `kube-vnet.system/managed-by=kube-vnet`, which every external-allow policy carries.
- New RBAC: `services` get/list/watch. Merged into the existing `namespaces` rule by controller-gen since they share the same verbs in the operator ClusterRole.
- Users who want a tighter source-CIDR allow for a specific Service: annotate `kube-vnet/external-allow=false` and write their own NetworkPolicy. The opt-out is per-resource so the rest of the namespace's Services still auto-emit.

## Alternatives considered

- **Extend the membership policy generator** to add an `ipBlock` from-rule when a Service exposes the membership's pods. Rejected: couples per-Service detection into the per-vnet generator, harder to reason about; mixing the "vnet membership says yes" and "external traffic says yes" rules into one policy obscures intent.
- **Opt-in with a Service annotation** instead of default-on. Rejected: most users hit the failure mode without knowing the opt-in exists. The whole point of an operator is that the right thing happens automatically; an opt-in shifts the failure into "silent breakage" mode.
- **Watch `Ingress` / Gateway API resources** as detection signals. Rejected as redundant — see "Why we don't watch Ingress or Gateway API" above.

## Out of scope

- **`hostNetwork: true` pods.** NetworkPolicy enforcement on host-network pods is CNI-dependent: Cilium does, kube-router doesn't, Calico is mode-dependent. Emitting a policy for them is unreliable. If you run a daemon on `hostNetwork: true`, exclude its namespace via `kube-vnet/disabled=true`.
- **`hostPort` container detection.** Same NetworkPolicy + node-IP source story, but per-pod policy emission needs a label-stamping design that's worth its own iteration. Deferred to a follow-up ADR if anyone hits it in practice.
- **Layer-7 / per-path policies.** Beyond NetworkPolicy semantics; not a kube-vnet concern.
- **Source-CIDR narrowing.** "Only allow from the LB's source range" is cloud-provider-specific. Users can opt out for that Service and write their own NetworkPolicy.

## Related ADRs

- **ADR 0006 — Baseline default-deny**. The "every managed NS gets a deny-all baseline" rule has an external-traffic blind spot that this ADR closes.
- **ADR 0030 — Unified vnet membership with resolution**. The membership policies' `namespaceSelector`-driven from-rules don't cover external sources; this ADR adds the missing layer.
- **ADR 0035 — Removal of `--elide-baseline-for`**. The flag was theatre because membership policies already allowed the relevant peers; this ADR adds the actually-missing layer for external traffic.
- **ADR 0036 — Helm pre-delete hook**. The cleanup hook's `kube-vnet.system/managed-by` selector automatically covers external-allow policies.
- **ADR 0037 — `kube-vnet.system/*` prefix for operator-owned keys**. The new `kube-vnet.system/role: external-allow` label sits under this prefix and inherits the system-labels VAP protection.
