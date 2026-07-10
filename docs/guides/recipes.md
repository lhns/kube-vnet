# Recipes

Worked end-to-end examples beyond the minimal samples in [`config/samples/`](../../config/samples/). Each recipe is self-contained — `kubectl apply -f -` the inline YAML and you'll have a working setup.

For the conceptual model behind these patterns, see [`concepts.md`](../getting-started/concepts.md).

> **Each recipe relies on the deny-all baseline + opt-in vnet membership model from [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md):** every managed namespace gets a deny-all baseline; pods that join a vnet (via the `kube-vnet/net.<vnet>` label) get additive ingress allows from vnet peers; everything else is denied. See the [deny-all baseline concept](../getting-started/concepts.md#the-deny-all-baseline) for details.

---

## Index

1. [Three-tier app: frontend → backend → database](#three-tier-app-frontend--backend--database)
2. [Shared observability network across all namespaces](#shared-observability-network-across-all-namespaces)
3. [Bridge pod joining two vnets (sidecar / proxy pattern)](#bridge-pod-joining-two-vnets-sidecar--proxy-pattern)
4. [Direction modes: ingress-only and egress-only members](#direction-modes-ingress-only-and-egress-only-members)
5. [Enrolling third-party pods via VirtualNetworkBinding](#enrolling-third-party-pods-via-virtualnetworkbinding)
6. [Migrating an existing namespace to kube-vnet](#migrating-an-existing-namespace-to-kube-vnet)
7. [Coexisting with user-managed `NetworkPolicy`](#coexisting-with-user-managed-networkpolicy)
8. [Per-workload egress allowlist via user-managed NetworkPolicy](#per-workload-egress-allowlist-via-user-managed-networkpolicy)
9. [Managing kube-system (and keeping DNS alive)](#managing-kube-system-and-keeping-dns-alive)

---

## Three-tier app: frontend → backend → database

A typical web application has three tiers, each only allowed to talk to the next:

- **frontend** — receives external traffic (Ingress / LoadBalancer). Talks to **backend**.
- **backend** — application logic. Talks to **database**.
- **database** — persistence. Receives from **backend** only.

In kube-vnet terms: two vnets, each connecting one tier to the next.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: webapp
---
# vnet 1: frontend ↔ backend
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: web-tier
  namespace: webapp
spec:
  description: |
    Frontend pods reach backend pods.
---
# vnet 2: backend ↔ database
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: data-tier
  namespace: webapp
spec:
  description: |
    Backend pods reach database pods.
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: frontend, namespace: webapp }
spec:
  replicas: 2
  selector: { matchLabels: { app: frontend } }
  template:
    metadata:
      labels:
        app: frontend
        kube-vnet/net.web-tier: "both"     # only on the web tier
    spec:
      containers:
        - { name: app, image: nginx:alpine }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: backend, namespace: webapp }
spec:
  replicas: 3
  selector: { matchLabels: { app: backend } }
  template:
    metadata:
      labels:
        app: backend
        kube-vnet/net.web-tier: "both"     # backend joins both
        kube-vnet/net.data-tier: "both"
    spec:
      containers:
        - { name: app, image: nginx:alpine }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: database, namespace: webapp }
spec:
  replicas: 1
  selector: { matchLabels: { app: database } }
  template:
    metadata:
      labels:
        app: database
        kube-vnet/net.data-tier: "both"    # only on the data tier
    spec:
      containers:
        - { name: db, image: postgres:16, env: [{ name: POSTGRES_PASSWORD, value: example }] }
```

Resulting connectivity:

- frontend ↔ backend (both on `web-tier`)
- backend ↔ database (both on `data-tier`)
- frontend ✗ database (no shared vnet — and the baseline blocks it)

Note that the **backend joins two vnets**. The labels are additive: NetworkPolicies select pods by `Exists <key>`, so a backend pod is matched by both the `web-tier` membership policy and the `data-tier` membership policy.

If you also want frontend to reach the internet, accept ingress from a LoadBalancer / Ingress controller, or restrict egress to specific destinations, that's separate. kube-vnet's baseline only restricts ingress; egress is unrestricted. Add a user-managed `NetworkPolicy` for ingress from external sources or for egress allowlists — it composes additively with kube-vnet's policies (see the last two recipes).

---

## Shared observability network across all namespaces

A scrape target (your application) exposes `/metrics`. Prometheus, in `monitoring`, needs to reach it. The vnet-driven model makes this clean: a single observability vnet that any namespace can join, with the scraper as a one-pod member.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: monitoring
---
apiVersion: v1
kind: Namespace
metadata:
  name: webapp
---
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: observability
  namespace: monitoring
spec:
  description: |
    Cluster-wide observability network. Pods in any namespace may join
    by adding the prefixed label kube-vnet/net.monitoring.observability=both.
  allowedNamespaces:
    all: true
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: prometheus, namespace: monitoring }
spec:
  replicas: 1
  selector: { matchLabels: { app: prometheus } }
  template:
    metadata:
      labels:
        app: prometheus
        # Bare form — prometheus is in the home namespace (monitoring).
        kube-vnet/net.observability: "both"
    spec:
      containers:
        - { name: prometheus, image: prom/prometheus:latest, ports: [{ containerPort: 9090 }] }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: webapp, namespace: webapp }
spec:
  replicas: 3
  selector: { matchLabels: { app: webapp } }
  template:
    metadata:
      labels:
        app: webapp
        # Prefixed form — webapp is in a different namespace.
        kube-vnet/net.monitoring.observability: "both"
    spec:
      containers:
        - { name: app, image: nginx:alpine }
```

Effect:

- Pods in **any namespace** that add `kube-vnet/net.monitoring.observability=both` become members of the observability network.
- Prometheus (in `monitoring`, with the bare label) and webapp (in `webapp`, with the prefixed label) can reach each other.
- Pods in `webapp` that *don't* add the label get nothing extra. The `allowedNamespaces.all: true` *permits* joining; it doesn't grant blanket access.

This is the canonical use case for `allowedNamespaces.all: true`. It's also useful for cluster-wide log forwarders, service-mesh control-plane sidecars, or anything else that needs to reach into many namespaces selectively.

---

## Bridge pod joining two vnets (sidecar / proxy pattern)

Two vnets that should not be reachable from each other directly, with a designated bridge pod that talks to both. Common cases: an API gateway in front of multiple backend services, an internal proxy, a translation sidecar.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: platform
---
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
spec: {}
---
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: monitoring
  namespace: platform
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: payments-api, namespace: platform }
spec:
  replicas: 2
  selector: { matchLabels: { app: payments-api } }
  template:
    metadata:
      labels:
        app: payments-api
        kube-vnet/net.payments: "both"
    spec:
      containers:
        - { name: app, image: nginx:alpine }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: monitoring-agent, namespace: platform }
spec:
  replicas: 1
  selector: { matchLabels: { app: monitoring-agent } }
  template:
    metadata:
      labels:
        app: monitoring-agent
        kube-vnet/net.monitoring: "both"
    spec:
      containers:
        - { name: agent, image: nginx:alpine }
---
# The bridge: joins both vnets, can talk to either.
apiVersion: apps/v1
kind: Deployment
metadata: { name: gateway, namespace: platform }
spec:
  replicas: 1
  selector: { matchLabels: { app: gateway } }
  template:
    metadata:
      labels:
        app: gateway
        kube-vnet/net.payments: "both"
        kube-vnet/net.monitoring: "both"
    spec:
      containers:
        - { name: gateway, image: envoyproxy/envoy:distroless-v1.31-latest }
```

Effect:

- gateway ↔ payments-api (both on `payments`)
- gateway ↔ monitoring-agent (both on `monitoring`)
- payments-api ✗ monitoring-agent (no shared vnet)

Same composition as the three-tier example: the bridge is just a pod that carries the join labels of both networks.

---

## Direction modes: ingress-only and egress-only members

The join label *value* declares which directions a pod participates in: `both` (default), `ingress`, `egress`, or `none`. Use this to model asymmetric workloads precisely instead of granting bidirectional access where only one direction is needed.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: platform
---
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata: { name: telemetry, namespace: platform }
spec: {}
---
# A read-only metrics API. Accepts queries; never initiates outbound to peers.
apiVersion: apps/v1
kind: Deployment
metadata: { name: metrics-api, namespace: platform }
spec:
  selector: { matchLabels: { app: metrics-api } }
  template:
    metadata:
      labels:
        app: metrics-api
        kube-vnet/net.telemetry: ingress    # accept-only
    spec:
      containers: [{ name: app, image: nginx:alpine }]
---
# A logging sidecar collector. Initiates outbound to the API; never accepts inbound.
apiVersion: apps/v1
kind: Deployment
metadata: { name: log-shipper, namespace: platform }
spec:
  selector: { matchLabels: { app: log-shipper } }
  template:
    metadata:
      labels:
        app: log-shipper
        kube-vnet/net.telemetry: egress     # initiate-only
    spec:
      containers: [{ name: app, image: nginx:alpine }]
---
# A regular service: bidirectional.
apiVersion: apps/v1
kind: Deployment
metadata: { name: app, namespace: platform }
spec:
  selector: { matchLabels: { app: app } }
  template:
    metadata:
      labels:
        app: app
        kube-vnet/net.telemetry: both       # default; explicit for clarity
    spec:
      containers: [{ name: app, image: nginx:alpine }]
```

Effect (the X→Y algebra: traffic flows iff X is `both`/`egress` AND Y is `both`/`ingress`):

- log-shipper → metrics-api: yes (egress sender + ingress receiver).
- log-shipper → app: yes.
- app → metrics-api: yes.
- metrics-api → log-shipper: no (metrics-api is ingress-only — never initiates).
- metrics-api → app: no (same reason).
- log-shipper → log-shipper (peers): no (egress-only — never accepts).

Generated policy in `platform`: one membership policy, `kube-vnet.mem.platform.telemetry-<8hex>`. It selects the receivers (pods stamped `both` or `ingress`) and allows ingress from the senders (pods stamped `both` or `egress`) via `In` matches on the stamped `kube-vnet.system/net.platform.telemetry` label. Egress-only members appear as allowed sources but are not themselves selected — nothing needs to allow ingress *to* them.

See [ADR 0021](../adr/0021-direction-modes-on-join-labels.md).

---

## Enrolling third-party pods via VirtualNetworkBinding

Some pods can't be labeled because their template comes from an upstream Helm chart (e.g. cert-manager, an Argo controller, a third-party operator). A `VirtualNetworkBinding` selects pods *in its own namespace* and attaches them to a target vnet without touching the pods.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: platform
---
apiVersion: v1
kind: Namespace
metadata:
  name: webapp
---
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata: { name: payments, namespace: platform }
spec:
  allowedNamespaces:
    names: [webapp]
---
# A binding LIVES in the namespace whose pods it selects.
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBinding
metadata:
  name: payments-thirdparty
  namespace: webapp
spec:
  virtualNetworkRef:
    name: payments
    namespace: platform
  direction: both                  # both | ingress | egress | none
  podSelector:
    matchLabels:
      app: thirdparty-billing-agent
```

Effect:

- The binding selects pods in `webapp` whose labels match `app: thirdparty-billing-agent`. Those pods are members of `platform/payments` for the binding's `direction` (default `both`).
- The binding's status reports `Ready=True, PodsAttached` (or `NoPodsMatch`/`VirtualNetworkNotFound`/`NamespaceNotAllowed`/etc; see [`troubleshooting.md`](troubleshooting.md#my-virtualnetworkbinding-doesnt-attach-any-pods)).
- The resolution controller stamps the canonical FQ system label `kube-vnet.system/net.platform.payments=both` on the selected pods. Those pods are then covered by the regular per-`(vnet, namespace)` membership policy in `webapp` named `kube-vnet.platform.payments-<8hex>` — no separate per-binding policy is emitted (per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)).

```bash
# Inspect bindings cluster-wide.
kubectl get vnb -A

# See which pods this binding attached.
kubectl get vnb -n webapp payments-thirdparty -o jsonpath='{.status.attachedPods}'

# Inspect the membership policy that covers those pods.
kubectl get networkpolicy -A -l kube-vnet.system/network=platform.payments
```

Constraints worth knowing:

- The selector is **scoped to the binding's own namespace**. There are no cross-namespace bindings.
- The target vnet's `spec.allowedNamespaces` is enforced. A binding in a non-permitted namespace surfaces `Ready=False, Reason=NamespaceNotAllowed`.
- Bindings in `kube-vnet/disabled` (or operator-excluded) namespaces are inert.

Bindings are an escape hatch — the join label is the recommended primary mechanism. Use them when you genuinely can't modify the pod template. See [ADR 0026](../adr/0026-virtualnetworkbinding-crd.md).

---

## Migrating an existing namespace to kube-vnet

You have a running namespace with workloads relying on default-allow Kubernetes networking. You want to bring it under kube-vnet without breaking anything in the middle.

The goal of the migration: at the end, every workload in the namespace either belongs to a vnet or is explicitly allowed by a user-managed NetworkPolicy. The baseline-tier model ([ADR 0031](../adr/0031-baseline-tier-resolution.md)) is what makes this safe to do gradually: install with the permissive `cluster` preset, label workloads at leisure, then tighten one namespace at a time with a per-namespace `VirtualNetworkBaseline`.

Step-by-step:

```bash
# 1. Make sure the operator is installed and healthy — ideally installed
#    with the adoption-friendly preset, so nothing is isolated yet:
#      --set operator.clusterBaseline.ingressIsolationLevel=cluster
kubectl get deploy -n kube-vnet-system kube-vnet-controller
kubectl get cvnbl default -o yaml    # confirm the seeded cluster baseline

# 2. Define the vnets your workloads need. Don't label any pods yet.
kubectl apply -f - <<EOF
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
spec: {}
EOF

# 3. Label workloads gradually. Under the `cluster` preset every pod still
#    inherits cluster=default-both, so adding a membership only ADDS
#    reachability (vnet peers reach each other); nothing loses traffic.
kubectl patch deployment orders -n platform --type=merge -p '
spec:
  template:
    metadata:
      labels:
        kube-vnet/net.payments: both
'

# 4. Verify the orders pods can reach what they need (other payments members)
#    and that nothing has regressed.

# 5. Repeat step 3 for each additional workload that should join.

# 6. Flip THIS namespace into the strict posture by overriding the cluster
#    default with a per-namespace VirtualNetworkBaseline. This is the moment
#    non-migrated pods in `platform` lose ingress reachability — everything
#    before this step is risk-free.
kubectl apply -f - <<EOF
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBaseline
metadata:
  name: default            # singleton per namespace
  namespace: platform
spec:
  memberships:
    - virtualNetworkRef: { name: namespace, namespace: kube-vnet-system }
      direction: none      # drop same-namespace blanket reachability
    - virtualNetworkRef: { name: cluster, namespace: kube-vnet-system }
      direction: egress    # keep outbound (DNS etc.); accept nothing by default
EOF

# 7. For workloads that need ingress NOT covered by any vnet (accepting
#    external Ingress controller traffic, etc.), add a user-managed
#    NetworkPolicy. kube-vnet's baseline + your allows compose additively.
#    (Externally-exposed Services and webhook backends are auto-allowed —
#    see the auto-allow guide.)
```

The key safety property: the isolation posture (baseline tier) is decoupled from membership. You can label pods first, watch traffic with no risk of breakage, and only tighten the namespace's baseline when you're ready. The per-namespace override in step 6 works because the `cluster` preset seeds `default-*` directions — advisory values a namespace baseline may override. Repeat step 6 per namespace as each one finishes migrating; when all namespaces are strict, you can tighten the cluster preset itself.

If migration will take a while, opt the namespace out of kube-vnet entirely while you work:

```bash
kubectl annotate namespace platform kube-vnet/disabled=true
# ... migrate workloads, label pods ...
kubectl annotate namespace platform kube-vnet/disabled-
```

Removing the annotation when you're done re-enables operator management. Until then the namespace stays in default-allow.

---

## Coexisting with user-managed `NetworkPolicy`

Not every connectivity need fits the membership model:

- An Ingress controller in `ingress-nginx` needs to reach pods in `webapp` (different namespace, no vnet relationship).
- An external monitoring agent in `monitoring` needs to scrape `/metrics` on a specific port.
- A pod needs to reach the public internet.

Kubernetes' NetworkPolicy is **additive**: a pod's allowed traffic is the union of allow-rules from every policy that selects it. So the operator's policies + your custom NetworkPolicy compose: the `kube-vnet.base` baseline denies ingress by default, the membership policies allow same-vnet traffic, and your NetworkPolicy adds specific allows on top.

Example: allow Ingress controller pods to reach webapp pods on port 80, alongside kube-vnet's vnet-driven isolation.

```yaml
# kube-vnet-managed: vnet for webapp services
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata: { name: webapp, namespace: webapp }
spec: {}
---
# kube-vnet-managed: workload joining the webapp vnet
apiVersion: apps/v1
kind: Deployment
metadata: { name: webapp, namespace: webapp }
spec:
  replicas: 2
  selector: { matchLabels: { app: webapp } }
  template:
    metadata:
      labels:
        app: webapp
        kube-vnet/net.webapp: "both"
    spec:
      containers:
        - { name: app, image: nginx:alpine, ports: [{ containerPort: 80 }] }
---
# USER-managed: in addition to the vnet's allow, also allow ingress from
# the ingress-nginx namespace on port 80.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-ingress-nginx
  namespace: webapp
  # Important: do NOT label this with kube-vnet.system/managed-by — that would
  # make the operator think it's its own and reset it on drift.
spec:
  podSelector:
    matchLabels:
      app: webapp
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
      ports:
        - { protocol: TCP, port: 80 }
```

The webapp pods are now reachable from:

- Other pods carrying `kube-vnet/net.webapp` (via the vnet's membership policy).
- Pods in `ingress-nginx` on port 80 (via your user policy).

And nothing else (the baseline denies the rest of the ingress; egress is unrestricted by kube-vnet — see the next recipe if you need to restrict it).

**Don't apply the `kube-vnet.system/managed-by=kube-vnet` label to your custom policies.** That label is the operator's claim of ownership; if your policy has it, the operator will treat it as drift on its own resource and may overwrite or delete it. User policies should have any other labels you want, just not that one.

If you accidentally pick a name kube-vnet wants to use (e.g. `kube-vnet` itself, or one of the operator-generated `kube-vnet.<vnet>-<hash>` shapes), the operator surfaces a `NameCollision` Degraded condition and refuses to overwrite — see [`troubleshooting.md`](troubleshooting.md). Rename your policy to resolve.

---

## Per-workload egress allowlist via user-managed NetworkPolicy

kube-vnet does not restrict egress. The operator's baseline is `policyTypes: [Ingress]` only; membership policies grant egress allows to vnet peers but don't restrict generic egress (DNS, the apiserver, the public internet, other namespaces). For workloads where outbound restriction matters, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]`.

Example: the `payments` deployment needs to reach (a) its vnet peers, (b) DNS via CoreDNS, and (c) Stripe's API. Nothing else.

```yaml
# kube-vnet manages the membership / ingress side. Egress to peers is allowed
# by the membership policy, but kube-vnet does NOT restrict other egress.
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata: { name: payments, namespace: platform }
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: payments-svc, namespace: platform }
spec:
  selector: { matchLabels: { app: payments-svc } }
  template:
    metadata:
      labels:
        app: payments-svc
        kube-vnet/net.payments: both
    spec:
      containers: [{ name: app, image: nginx:alpine }]
---
# USER-MANAGED. Egress allowlist for payments-svc:
# - DNS to CoreDNS in kube-system.
# - Stripe API (cluster has a deterministic egress IP — example only).
# Egress to vnet peers is already permitted by the membership policy.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: payments-svc-egress
  namespace: platform
  # Important: do NOT label with kube-vnet.system/managed-by.
spec:
  podSelector:
    matchLabels:
      app: payments-svc
  policyTypes: [Egress]
  egress:
    # DNS
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
    # Stripe API. Replace the CIDR with the actual destination (or use a
    # CNI-extension policy with FQDN matching for real-world Stripe egress).
    - to:
        - ipBlock:
            cidr: 203.0.113.0/24
      ports:
        - { protocol: TCP, port: 443 }
```

Once any policy selects a pod for `policyTypes: [Egress]`, that pod's egress goes default-deny — only allow rules across all selecting policies are permitted. The membership policy contributes its peer allow; your user policy contributes DNS and Stripe; everything else is blocked.

For threat-model considerations and the broader case for keeping kube-vnet's scope ingress-only, see [`security.md`](../security/security.md) and [ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md). Cluster-level egress firewalls (Calico GlobalNetworkPolicy, Cilium FQDN policy, NAT-gateway allowlists, service-mesh egress proxies) are often the right answer for the cluster-boundary case.

---

## Managing kube-system (and keeping DNS alive)

By default `kube-system` is in `operator.disabledNamespaces`, so kube-vnet stays out of it entirely. To bring its pods under management — segment them into vnets, or just stop carving a hole in an otherwise-managed cluster — remove it from the list:

```yaml
# values.yaml
operator:
  disabledNamespaces: []   # manage everything, including kube-system
```

The catch: CoreDNS's inbound `:53` is a plain ClusterIP, matched by none of the [auto-allow families](auto-allow.md). Under the deny-all baseline it would go dark and **cluster DNS would break everywhere**. The chart prevents this automatically — the moment `kube-system` is no longer disabled, it renders a `NetworkPolicy` that re-opens `:53`:

```bash
kubectl get netpol -n kube-system kube-vnet-coredns-allow -o yaml
```

```yaml
spec:
  podSelector:
    matchLabels: { k8s-app: kube-dns }
  policyTypes: [Ingress]
  ingress:
    - from: [{ ipBlock: { cidr: 0.0.0.0/0 } }]
      ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
```

`0.0.0.0/0` is deliberate: every pod queries DNS regardless of vnet membership, and hostNetwork clients query it from the *node* IP — universal reachability a vnet binding can't express (see [ADR 0042](../adr/0042-coredns-ingress-carveout-and-kube-system-enrollment.md) for why a binding is the wrong tool here). Tune the selector, namespace, or ports (e.g. add `9153` for metrics) via the `dnsCarveout.*` [values](../reference/configuration.md#dnscarveout-coredns-ingress-carve-out--adr-0042); set `dnsCarveout.enabled: false` to opt out.

What about the rest of `kube-system`? hostNetwork pods (kube-proxy, CNI agents, konnectivity) are skipped by kube-vnet, and metrics-server is reached by the apiserver via the [`ext.apiserver`](auto-allow.md#apiserver-reachable-services-extapiserver) family. On distributions with additional ClusterIP services in `kube-system` that other pods consume, verify reachability after enrolling — those may need a vnet or a user-managed `NetworkPolicy` of their own.
