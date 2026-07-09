# Auto-allow: traffic the operator admits without being asked

kube-vnet's deny-all baseline selects every pod in a managed namespace. `NetworkPolicy` allows are expressed as pod/namespace selectors — which means three whole categories of *legitimate* traffic would be silently broken, because their source is never a pod that any selector can match:

1. **External clients** reaching a LoadBalancer/NodePort Service — the packet arrives SNAT'd to a node IP or with the original client IP; neither lives in any namespace.
2. **Anything hitting a `hostPort`** — same story, the node IP is the destination and sources are arbitrary.
3. **The kube-apiserver calling into the cluster** — admission webhooks, aggregated API servers, CRD conversion webhooks. The apiserver isn't a pod; on many distributions its calls arrive from a control-plane or node IP.

Rather than making you hand-write brittle `ipBlock` policies (or disable kube-vnet for entire namespaces), the operator detects these cases from the resources that declare them and emits a dedicated, clearly-labeled allow for each. This page is the complete model; the [FAQ](../faq.md) and [troubleshooting](troubleshooting.md) pages carry the symptom-side entries.

Design records: [ADR 0038](../adr/0038-auto-allow-externally-exposed-services.md) (Services), [ADR 0040](../adr/0040-auto-allow-hostport-pods.md) (hostPort), [ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md) (apiserver).

## How to recognize them

Every auto-allow policy is named `kube-vnet.ext.<source-kind>.<identity>-<8hex>` and labeled for querying:

```bash
kubectl get networkpolicy -A -l kube-vnet.system/role=external-allow
kubectl get networkpolicy -A -l kube-vnet.system/source-kind=apiserver   # one family
```

| Label | Values |
|---|---|
| `kube-vnet.system/role` | `external-allow` |
| `kube-vnet.system/source-kind` | `svc` \| `host` \| `apiserver` |
| `kube-vnet.system/source` | `svc-<name>` \| `host-<port>-<proto>` \| `apiserver-<name>` |

All three families are **additive and port-scoped**: NetworkPolicy union semantics mean they open exactly the declared port(s) on exactly the backing pods, while every other port on those pods — and every other pod — keeps its baseline/membership protection.

## Externally-exposed Services (`ext.svc`)

**Trigger** — a Service whose spec declares external reachability:

| Service shape | Emits? |
|---|---|
| `type: LoadBalancer` | yes |
| `type: NodePort` | yes |
| `type: ClusterIP` (or unset) with non-empty `spec.externalIPs` | yes |
| plain `ClusterIP`, headless (`clusterIP: None`), `ExternalName`, or no `spec.selector` | no |

**Emitted** — `kube-vnet.ext.svc.<service>-<8hex>` in the Service's namespace: podSelector copied from the Service's selector, ingress `from: ipBlock 0.0.0.0/0` on the Service's **targetPort(s)** (that's the port the packet actually carries after kube-proxy DNAT — allowing the nodePort would match nothing). Named targetPorts are resolved against the backing pods; until a matching pod exists the policy is held back and a `Pending` event is emitted on the Service.

The policy carries an owner reference to the Service, so deleting the Service cascades the policy away.

**Opt out** — per Service or per namespace:

```bash
kubectl annotate svc <name> -n <ns> kube-vnet/external-allow=false
kubectl annotate ns <ns> kube-vnet/external-allow=false
```

Only the literal value `"false"` opts out. Opting out means you take over: write your own NetworkPolicy for the exposure or accept unreachability.

## hostPort pods (`ext.host`)

**Trigger** — any pod declaring `hostPort` on a container port. One policy per distinct `(namespace, port, protocol)` — stable across pod restarts and rollouts, because the identity is the port, not the pod.

**Emitted** — `kube-vnet.ext.host.<port>.<proto>-<8hex>`: ingress `ipBlock 0.0.0.0/0` on that port/protocol, selecting pods via an operator-stamped marker label `kube-vnet.system/host-port.<port>.<proto>=true` (the resolution controller stamps it on every pod declaring that hostPort; the stamp is VAP-protected like all `kube-vnet.system/*` keys).

`hostNetwork: true` pods are skipped — NetworkPolicy enforcement on host-network pods is CNI-dependent, so a policy would promise nothing.

**Opt out** — namespace-wide only (`kube-vnet/external-allow=false` or `kube-vnet/disabled=true` on the namespace); hostPort has no Service object to annotate.

## Apiserver-reachable Services (`ext.apiserver`)

The subtlest case. When you install cert-manager (or kyverno, gatekeeper, metrics-server, …), the kube-apiserver itself must call a Service in your cluster. Without an allow, admission requests time out:

```
Error from server (InternalError): ... failed calling webhook "webhook.cert-manager.io":
Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s":
context deadline exceeded
```

**Trigger** — the operator watches the four cluster-scoped resources that declare "the apiserver dials this Service":

| Discovery resource | Field | Typical owners |
|---|---|---|
| `ValidatingWebhookConfiguration` | `webhooks[].clientConfig.service` | cert-manager, kyverno, gatekeeper |
| `MutatingWebhookConfiguration` | `webhooks[].clientConfig.service` | istio injector, cert-manager |
| `APIService` | `spec.service` | metrics-server, custom-metrics |
| `CustomResourceDefinition` | `spec.conversion.webhook.clientConfig.service` | multi-version CRDs |

Entries using `clientConfig.url` (out-of-cluster webhooks) are ignored — no in-cluster policy can help them.

**Emitted** — `kube-vnet.ext.apiserver.<service>-<8hex>` in the Service's namespace, allowing ingress on the referenced targetPort(s) from `operator.apiserverSourceCIDR`.

**The CIDR knob** — default `0.0.0.0/0`. That sounds wide, but it matches the posture the webhook had *before* kube-vnet existed (default Kubernetes is allow-all), and webhook/APIService backends authenticate their callers at the TLS layer (caBundle / requestheader CA) — the network allow is not the security boundary. If your pod network is reachable from outside the cluster and you want a narrower allow:

```yaml
operator:
  apiserverSourceCIDR: "10.1.2.0/24"   # your control-plane subnet
```

(Validated at operator startup; an unparseable CIDR fails fast.)

**Opt in** — for a Service the four discovery kinds don't cover (a future API, a custom operator's callback):

```bash
kubectl annotate svc <name> -n <ns> kube-vnet/apiserver-reachable=true
```

**Opt out** — same annotation as the other families: `kube-vnet/external-allow=false` on the Service or namespace.

## Cluster DNS when kube-system is managed

Not an operator family — a sibling worth knowing about. CoreDNS's `:53` is a plain ClusterIP, matched by none of the families above. So if you enroll `kube-system` (remove it from `operator.disabledNamespaces`), the deny-all baseline would black-hole cluster DNS.

The **chart** handles this, not the operator: it ships a `NetworkPolicy` (`kube-vnet-coredns-allow`) that re-opens `:53` from `0.0.0.0/0`, rendered automatically whenever the DNS namespace is managed. DNS needs *universal* reachability — every pod, plus hostNetwork clients querying from the node IP — which a vnet binding can't express (membership only reaches co-members), so it's a raw `ipBlock` policy like the families above. Configure via the `dnsCarveout.*` [chart values](../reference/configuration.md#dnscarveout-coredns-ingress-carve-out--adr-0042); details in [ADR 0042](../adr/0042-coredns-ingress-carveout-and-kube-system-enrollment.md).

## Composition and coexistence

- A Service can trigger **more than one family** — a LoadBalancer Service that's also a webhook backend gets both `ext.svc.*` and `ext.apiserver.*` policies. They coexist; union semantics make the result exactly what each declares.
- Auto-allow never widens pod-to-pod reachability: vnet membership rules are untouched, and ports not declared by a Service/hostPort/webhook stay protected.
- Everything is swept automatically when its trigger disappears (Service deleted, hostPort removed, webhook configuration dropped, annotation flipped) and drift-corrected if deleted by hand.

## Troubleshooting pointers

- Webhook still timing out with the policy present → [troubleshooting § admission webhook fails](troubleshooting.md#admission-webhook-fails-with-context-deadline-exceeded) (walks kube-proxy, konnectivity, and CNI-layer causes).
- LoadBalancer unreachable → [operations playbook](operations.md#my-loadbalancer--nodeport-service-isnt-reachable-from-outside-the-cluster).
- Errors that *change shape between attempts* usually mean a flaky control-plane component, not policy — see the diagnostic ladder in troubleshooting.
