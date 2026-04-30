# Recipes

Worked end-to-end examples beyond the minimal samples in [`config/samples/`](../config/samples/). Each recipe is self-contained — `kubectl apply -f -` the inline YAML and you'll have a working setup.

For the conceptual model behind these patterns, see [`concepts.md`](concepts.md).

---

## Index

1. [Three-tier app: frontend → backend → database](#three-tier-app-frontend--backend--database)
2. [Shared observability network across all namespaces](#shared-observability-network-across-all-namespaces)
3. [Bridge pod joining two vnets (sidecar / proxy pattern)](#bridge-pod-joining-two-vnets-sidecar--proxy-pattern)
4. [Migrating an existing namespace to kube-vnet](#migrating-an-existing-namespace-to-kube-vnet)
5. [Coexisting with user-managed `NetworkPolicy`](#coexisting-with-user-managed-networkpolicy)

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
        kube-vnet/net.web-tier: "true"     # only on the web tier
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
        kube-vnet/net.web-tier: "true"     # backend joins both
        kube-vnet/net.data-tier: "true"
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
        kube-vnet/net.data-tier: "true"    # only on the data tier
    spec:
      containers:
        - { name: db, image: postgres:16, env: [{ name: POSTGRES_PASSWORD, value: example }] }
```

Resulting connectivity:

- frontend ↔ backend (both on `web-tier`)
- backend ↔ database (both on `data-tier`)
- frontend ✗ database (no shared vnet — and the baseline blocks it)

Note that the **backend joins two vnets**. The labels are additive: NetworkPolicies select pods by `Exists <key>`, so a backend pod is matched by both the `web-tier` membership policy and the `data-tier` membership policy.

If you also want frontend to reach the internet, ingress traffic from a LoadBalancer/Ingress controller, or any other external destination, that's separate. kube-vnet's baseline only allows DNS egress; everything else is denied. Add a user-managed `NetworkPolicy` for those — it composes additively with kube-vnet's policies (see the last recipe).

---

## Shared observability network across all namespaces

A scrape target (your application) exposes `/metrics`. Prometheus, in `monitoring`, needs to reach it. The vnet-driven model makes this clean: a single observability vnet that any namespace can join, with the scraper as a one-pod member.

```yaml
apiVersion: v1
kind: Namespace
metadata: { name: monitoring }
---
apiVersion: v1
kind: Namespace
metadata: { name: webapp }
---
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: observability
  namespace: monitoring
spec:
  description: |
    Cluster-wide observability network. Pods in any namespace may join
    by adding the prefixed label kube-vnet/net.monitoring.observability=true.
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
        kube-vnet/net.observability: "true"
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
        kube-vnet/net.monitoring.observability: "true"
    spec:
      containers:
        - { name: app, image: nginx:alpine }
```

Effect:

- Pods in **any namespace** that add `kube-vnet/net.monitoring.observability=true` become members of the observability network.
- Prometheus (in `monitoring`, with the bare label) and webapp (in `webapp`, with the prefixed label) can reach each other.
- Pods in `webapp` that *don't* add the label get nothing extra. The `allowedNamespaces.all: true` *permits* joining; it doesn't grant blanket access.

This is the canonical use case for `allowedNamespaces.all: true`. It's also useful for cluster-wide log forwarders, service-mesh control-plane sidecars, or anything else that needs to reach into many namespaces selectively.

---

## Bridge pod joining two vnets (sidecar / proxy pattern)

Two vnets that should not be reachable from each other directly, with a designated bridge pod that talks to both. Common cases: an API gateway in front of multiple backend services, an internal proxy, a translation sidecar.

```yaml
apiVersion: v1
kind: Namespace
metadata: { name: platform }
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
        kube-vnet/net.payments: "true"
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
        kube-vnet/net.monitoring: "true"
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
        kube-vnet/net.payments: "true"
        kube-vnet/net.monitoring: "true"
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

## Migrating an existing namespace to kube-vnet

You have a running namespace with workloads relying on default-allow Kubernetes networking. You want to bring it under kube-vnet without breaking anything in the middle.

The goal of the migration: at the end, every workload in the namespace either belongs to a vnet or is explicitly allowed by a user-managed NetworkPolicy. The default-deny baseline is what makes this real.

Step-by-step:

```bash
# 1. Make sure the operator is installed and healthy.
kubectl get deploy -n kube-vnet-system kube-vnet-controller

# 2. Define the vnets your workloads need. Don't label any pods yet.
kubectl apply -f - <<EOF
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
spec: {}
EOF

# 3. Add the join label to ONE workload first. The reconciler installs the
#    membership policy + the baseline at this moment.
kubectl patch deployment orders -n platform --type=merge -p '
spec:
  template:
    metadata:
      labels:
        kube-vnet/net.payments: "true"
'

# 4. Verify the orders pods can reach what they need (other payments
#    members, DNS) and CAN'T reach what they shouldn't.

# 5. Repeat step 3 for each additional workload that should join.

# 6. For workloads that need connectivity NOT covered by any vnet (e.g.
#    accepting external Ingress traffic, scraping a third-party API), add
#    a USER-MANAGED NetworkPolicy. kube-vnet's baseline + your allows
#    compose additively.
```

The key safety property: the moment the *first* pod in the namespace joins a vnet, the deny baseline lands. Every other pod in the namespace that didn't get migrated is now isolated. This is the right behavior — but it means you should be ready to either (a) migrate all workloads quickly, (b) add explicit user-managed NetworkPolicies for the legacy ones, or (c) move legacy workloads to a different namespace temporarily.

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

Kubernetes' NetworkPolicy is **additive**: a pod's allowed traffic is the union of allow-rules from every policy that selects it. So the operator's baseline + your custom NetworkPolicy compose: the baseline says "deny everything except DNS"; your NetworkPolicy adds specific allows on top.

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
        kube-vnet/net.webapp: "true"
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
  # Important: do NOT label this with kube-vnet/managed-by — that would
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

And nothing else (the baseline denies the rest).

The same pattern works for egress — add a user-managed NetworkPolicy with `policyTypes: [Egress]` selecting your pods and `to:` rules for the destinations they need.

**Don't apply the `kube-vnet/managed-by=kube-vnet` label to your custom policies.** That label is the operator's claim of ownership; if your policy has it, the operator will treat it as drift on its own resource and may overwrite or delete it. User policies should have any other labels you want, just not that one.

If you accidentally pick a name kube-vnet wants to use (e.g. `kube-vnet-default-deny` or `kube-vnet-<your-vnet>-<ns>`), the operator surfaces a `NameCollision` Degraded condition and refuses to overwrite — see [`troubleshooting.md`](troubleshooting.md). Rename your policy to resolve.
