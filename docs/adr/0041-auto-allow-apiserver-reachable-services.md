# ADR 0041 — Auto-allow Services reached by the apiserver

> **Amendment (2026-06-29, second same-day follow-up)**: the emitted policy's `from:` rule now includes BOTH `ipBlock: <apiserverSourceCIDR>` AND `namespaceSelector: {}` (empty selector → all namespaces). Many CNIs (Calico in some configs, Cilium, kube-router) treat `ipBlock` peers as off-cluster-source-only and do NOT match cluster-internal IPs against them — even `ipBlock: 0.0.0.0/0`. On k0s / kubeadm-with-Konnectivity / GKE private clusters the apiserver reaches webhook backends via a konnectivity-agent that presents node-IP-sourced traffic; without the namespaceSelector peer, that traffic was being dropped despite the apparent allow-all (user-reported `No agent available` after the named-port fix). The `namespaceSelector: {}` makes the policy CNI-portable. Webhook servers still authenticate the apiserver via caBundle/requestheader CA at the TLS layer, so the broader peer set doesn't compromise security.

> **Amendment (2026-06-29, same-day follow-up)**: the initial implementation emitted policies scoped to the Service-side port rather than the pod-side targetPort. NetworkPolicy is enforced after kube-proxy DNATs to `pod:targetPort`, so a Service-port allow doesn't actually permit the apiserver's traffic — admission silently times out. Symptom on the user's cluster:
>
> ```
> kube-vnet.ext.apiserver.cert-manager-webhook-943e7fca   To Port: 443/TCP   ← wrong
> kubectl apply -f certs.yaml → context deadline exceeded                    ← still broken
> ```
>
> Fixed by reusing `resolveTargetPort` from `external_allow_controller.go` (ADR 0038) — walks backing pods, finds the containerPort whose name matches the Service's `targetPort: <name>`, returns its number (10250 for cert-manager-webhook). Pod watcher added to re-trigger emission when a previously-missing backing pod appears. Builder now returns `(policy, error)`; caller emits a Pending Event + 30s requeue on `errNamedPortUnresolvable`. The original anti-test (`TestBuildApiserverReachablePolicy_NamedTargetPortFallback`) that enshrined the broken behavior was removed; replaced with three tests asserting the correct contract plus two integration tests for the cert-manager-shape and the Pod-create-unblocks-Pending flow.

**Status**: Accepted (2026-06-29)

## Context

NetworkPolicy doesn't naturally express "the kube-apiserver is allowed to dial this Service." Pod-to-pod allows rely on `namespaceSelector` + `podSelector`; external traffic to a LoadBalancer uses `ipBlock` (ADR 0038); hostPort traffic uses pod-level `ipBlock` (ADR 0040). But the apiserver itself isn't a pod in any user namespace — it's a static pod on the control-plane node (kubeadm), a process inside the k0s/k3s controller binary, or a managed-control-plane endpoint (GKE/EKS/AKS). Its source IP from a webhook backend's perspective is either the node IP or a managed-IP, and neither matches any selector kube-vnet emits.

Concrete user symptom this ADR closes:

> User installs cert-manager. cert-manager ships a `ValidatingWebhookConfiguration` pointing at `cert-manager-webhook.cert-manager.svc:443`. On every `kubectl apply` of a `Certificate` (or `Issuer`, `ClusterIssuer`, etc.), the apiserver dials the webhook. With kube-vnet's baseline + per-vnet membership policies active, the webhook pod's deny-all baseline rejects the connection — there's no `from:` rule matching the apiserver's source IP. After ~30s the admission webhook times out with `failed calling webhook "webhook.cert-manager.io": context deadline exceeded`.

The same shape recurs across every component that ships an admission webhook, an aggregated API server, or a CRD conversion webhook: gatekeeper, kyverno, kube-prometheus-stack admission, istio sidecar injector, vault webhook, sigstore policy, metrics-server, custom-metrics-apiserver, knative-serving, kueue, kubevirt, gateway-api, and a long tail of niche operators.

ADR 0038 closes the external-to-Service gap (`type: LoadBalancer / NodePort / ClusterIP + externalIPs`). ADR 0040 closes the external-to-hostPort gap. This ADR closes the apiserver-to-internal-Service gap — the in-cluster non-pod-source half.

Existing escape hatches before this ADR:

- `kube-vnet/disabled=true` on the cert-manager namespace — turns kube-vnet off entirely. Loses pod-to-pod isolation. Doesn't scale as users install more webhook-providing components.
- Hand-written `NetworkPolicy` with `ipBlock: 0.0.0.0/0` per webhook Service — works but brittle: chart upgrades that rename labels or rotate ports silently break the hand-written allow.

Both lose information the operator already has: the apiserver's discovery resources (`ValidatingWebhookConfiguration`, `MutatingWebhookConfiguration`, `APIService`, `CustomResourceDefinition.spec.conversion`) name exactly which Services the apiserver dials. The operator can emit the right NetworkPolicy without the user writing it.

## Decision

A new `ApiserverReachableReconciler` watches four cluster-scoped Kubernetes resources that declare "the apiserver dials this Service" and emits one external-allow `NetworkPolicy` per `(Service NS, Service name)`, scoped to the discovery-referenced port(s). The emitted policy is additive — shares the namespace with kube-vnet's baseline + membership policies and the ADR 0038 / 0040 external-allow policies, composing via NetworkPolicy union semantics. Pod-to-pod isolation via vnet membership keeps working; the apiserver gets a narrow path to the webhook's targetPort.

### What triggers an emission

| Source resource | Detection field path | Common consumers |
|---|---|---|
| `admissionregistration.k8s.io/v1.ValidatingWebhookConfiguration` | `webhooks[].clientConfig.service` | cert-manager, kyverno, gatekeeper, kube-prometheus-stack, vault |
| `admissionregistration.k8s.io/v1.MutatingWebhookConfiguration` | `webhooks[].clientConfig.service` | cert-manager mutator, istio sidecar injector, kyverno mutator, sigstore policy |
| `apiregistration.k8s.io/v1.APIService` | `spec.service` | metrics-server, custom-metrics-apiserver, knative-serving, kueue, kubevirt, gateway-api |
| `apiextensions.k8s.io/v1.CustomResourceDefinition` | `spec.conversion.webhook.clientConfig.service` | CRDs with multi-version conversion (rare) |
| any `corev1.Service` with annotation `kube-vnet/apiserver-reachable=true` | watch `corev1.Service` | escape hatch for future K8s APIs / custom CRDs / ad-hoc |

URL-only `clientConfig.url` entries are skipped — they're out-of-cluster and irrelevant for NetworkPolicy. Local APIServices (`spec.service: nil` — the apiserver hosts the API itself) are skipped. Conversion strategy `None` skips the CRD.

### The emitted policy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.ext.apiserver.<svcName>-<8hex>
  namespace: <svcNS>
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/role: external-allow
    kube-vnet.system/source-kind: apiserver
    kube-vnet.system/source: apiserver-<svcName>
  ownerReferences:
    - kind: Service        # apiserver GC cascades on Service delete
      name: <svcName>
      controller: true
      blockOwnerDeletion: true
spec:
  podSelector:
    matchLabels: { ...verbatim from Service.spec.selector }
  ingress:
    - from:
        - ipBlock: { cidr: <operator.apiserverSourceCIDR | default 0.0.0.0/0> }
      ports:
        - protocol: TCP
          port: <Service.spec.ports[i].targetPort matching the discovery port>
  policyTypes: [Ingress]
```

Three design points worth calling out:

1. **`from: ipBlock <apiserverSourceCIDR>`** — default `0.0.0.0/0` matches the cluster's no-NetworkPolicy baseline (in plain K8s, anyone in the cluster can reach any Service on any port, so a `0.0.0.0/0` ingress allow on the webhook port doesn't make things worse than the default state). Webhook servers and APIServices authenticate the apiserver via TLS+CA bundle (`caBundle` for webhooks; `requestheader-client-ca-file` for APIServices) — transport-layer auth makes wide CIDRs safe in practice. Admins who want tighter set `operator.apiserverSourceCIDR` to their control-plane subnet (e.g. `10.1.2.0/24`).
2. **Port-scoped, always to `targetPort`** — same reasoning as ADR 0038. kube-proxy DNATs Service-port → pod-targetPort before NetworkPolicy evaluation. Admin/metrics ports on the same pod (not declared in the Service spec) stay protected.
3. **Owner reference to the Service** — same-NS cascade-delete works via apiserver GC, no reconciler needed when the Service goes away. Multiple discovery resources can reference the same Service; the policy stays as long as ANY discovery resource (or the annotation) references it.

### Opt-out

Same `kube-vnet/external-allow=false` annotation as ADR 0038 — single vocabulary across the auto-allow family. Per Service:

```bash
kubectl annotate -n cert-manager svc cert-manager-webhook kube-vnet/external-allow=false
```

Or per Namespace (disables auto-emit for every Service in the NS):

```bash
kubectl annotate ns cert-manager kube-vnet/external-allow=false
```

### Why not narrower than `0.0.0.0/0` by default

Three narrowing options were considered:

| | Approach | Distribution support | Implementation cost |
|---|---|---|---|
| (a) | `podSelector` matching the apiserver Pod | kubeadm yes (component=kube-apiserver label); k0s/k3s no (apiserver is a process); managed clusters no (apiserver isn't in user cluster) | small but unreliable |
| (b) | `ipBlock` matching control-plane node IPs (watch `corev1.Node`) | every distribution where apiserver runs on cluster nodes; doesn't help managed clusters | medium; re-emit on Node changes |
| (c) | Admin-configurable `operator.apiserverSourceCIDR` chart value | universal | trivial |

(c) is the only option that works across kubeadm / k0s / k3s / GKE / EKS / AKS uniformly. Ship the configurable knob; default to `0.0.0.0/0`.

### Coexistence

**With ADR 0038 (LoadBalancer / NodePort / externalIPs)**: a Service can be both LB-exposed AND webhook-referenced. Both reconcilers emit policies (`ext.svc.<name>` and `ext.apiserver.<name>` — different names, different `source-kind` labels). Union-of-allows means same effective behavior as one alone. No conflict.

**With ADR 0040 (hostPort pods)**: orthogonal — different source-kind, different policy shape, no overlap.

### Naming + label vocabulary (per ADR 0039)

- Policy name: `kube-vnet.ext.apiserver.<svcName>-<8hex>`. Total ≤63 chars; hash over `<ns>/<name>` for cross-NS uniqueness on truncated bases.
- `kube-vnet.system/source-kind: apiserver` — new value alongside `svc` and `host`.
- `kube-vnet.system/source: apiserver-<svcName>` — symmetric with `svc-<name>` and `host-<port>-<proto>`.
- Annotation (opt-in escape hatch): `kube-vnet/apiserver-reachable=true` on a Service.
- Annotation (opt-out, shared with ADR 0038): `kube-vnet/external-allow=false` on a Service or NS.
- Operator flag: `--apiserver-source-cidr` (default `0.0.0.0/0`). Validated as a parseable CIDR at startup.
- Chart value: `operator.apiserverSourceCIDR`.
- RBAC: get/list/watch on the four discovery resource kinds (cluster-scoped).

## Alternatives considered

1. **`kube-vnet/disabled=true` per webhook-providing NS** (the status-quo workaround). Loses pod-to-pod isolation in those NSes. Doesn't scale across system components.
2. **Hand-written `NetworkPolicy` per webhook**. Brittle to chart upgrades.
3. **Auto-allow `0.0.0.0/0` for every Service regardless of discovery**. Discussed thoroughly; security regression for ClusterIP DBs (Redis, Postgres, Elasticsearch) and admin-endpoints that rely on NetworkPolicy as their only auth layer. The transport-layer-auth safety argument only holds for the discovery-resource cases.
4. **Single-kind reconciler (just `ValidatingWebhookConfiguration`)**. Partial coverage; users with `MutatingWebhookConfiguration` (istio) or `APIService` (metrics-server) hit the same wall and reasonably expect kube-vnet to handle webhook backends uniformly.
5. **Node-watching to auto-narrow the source CIDR**. Considered; deferred. The admin-configurable `apiserverSourceCIDR` covers the same need with less code and works across distributions where the apiserver isn't on the cluster's nodes.

## Out of scope

- Kubelet-sourced probe traffic (liveness/readiness). Source IP varies per node; CNI defaults handle this on Calico/Cilium/Flannel. Documented as a troubleshooting entry; not auto-allowed.
- Prometheus / monitoring scrape traffic. Source IS a pod; standard NetworkPolicy via `namespaceSelector` works; users compose with kube-vnet's policies or add Prometheus to a cluster vnet.
- Service mesh control plane → sidecar traffic. Mesh manages its own policy layer.
- DNS responses to outbound pod queries. Conntrack ESTABLISHED handles this; not a new connection.

## Consequences

- **Positive**: cert-manager / kyverno / gatekeeper / metrics-server / kube-prometheus-stack admission / istio sidecar injector / etc. work on every kube-vnet cluster with zero user config.
- **Positive**: admins get a single knob (`operator.apiserverSourceCIDR`) for cluster-wide tightening.
- **Positive**: future K8s APIs that surface a Service ref are coverable via the `kube-vnet/apiserver-reachable=true` annotation without an operator update.
- **Negative**: users with webhook-providing components now also have a `kube-vnet.ext.apiserver.*` NetworkPolicy they didn't write. Documented in FAQ + troubleshooting.
- **Negative**: default `0.0.0.0/0` is permissive on the webhook port. Matches K8s default state; webhook servers TLS-auth their clients; documented as a known tradeoff with the narrowing option.

## References

- [ADR 0038 — Auto-allow externally-exposed Services](0038-auto-allow-externally-exposed-services.md) (LoadBalancer/NodePort/externalIPs counterpart)
- [ADR 0039 — Uniform kind-prefixed policy naming](0039-uniform-kind-prefixed-policy-naming.md) (naming convention)
- [ADR 0040 — Auto-allow hostPort pods](0040-auto-allow-hostport-pods.md) (hostPort counterpart)
