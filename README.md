# kube-vnet

A Kubernetes operator that lets you declare **named networks** as a first-class resource. Services join a network with a label; the operator generates the underlying `NetworkPolicy` resources so only same-network pods can talk to each other.

---

## Why?

By default, every pod in a Kubernetes cluster can reach every other pod. Most teams want the opposite — services should only reach the services they explicitly need.

`NetworkPolicy` exists to tighten this, but it's awkward to use directly. You write rules in terms of label selectors and exceptions ("allow ingress from pods with these labels in those namespaces"), which doesn't match how teams actually reason about connectivity. The natural mental model is the other way around: *"the payments service joins the payments network; so does orders; so do their dependencies. Nothing else can reach them."*

`kube-vnet` flips the model. You declare a `VirtualNetwork`. Services join it by adding a label. The operator emits the underlying `NetworkPolicy` set — and an automatic default-deny baseline so non-members are actually isolated, not just decoratively excluded.

The output is plain `networking.k8s.io/v1` `NetworkPolicy`. No CNI extensions, no lock-in. If you ever uninstall the operator, the policies it generated keep working.

## The mental model

If you've used Docker Swarm: a `VirtualNetwork` is the same idea — a named group that services join. Same-network pods can communicate; pods on different networks (or none) cannot.

The "virtual" qualifier is deliberate: there's no separate network plane. Traffic still flows through whatever CNI your cluster runs. The operator just shapes the `NetworkPolicy` set so connectivity follows membership.

## How it works (a worked example)

You declare a network:

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
```

You label pods that should join it:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: orders, namespace: platform }
spec:
  template:
    metadata:
      labels:
        app: orders
        kube-vnet/net.payments: "both"   # ← join the payments network
    spec:
      containers: [{ name: app, image: nginx:alpine }]
```

The operator notices and produces, in the `platform` namespace:

- `kube-vnet.mem.platform.payments-<8hex>` — a `NetworkPolicy` selecting any pod with the operator-stamped `kube-vnet.system/net.platform.payments` label, allowing ingress from peers in the same vnet (filtered by their declared direction).
- `kube-vnet.base` — a uniform deny-all ingress baseline, installed in every managed namespace, so non-members aren't reachable. Egress is unrestricted by kube-vnet — for per-workload egress restriction, add a user-managed `NetworkPolicy` with `policyTypes: [Egress]`.

The picture:

```
┌─ namespace: platform ──────────────────────────────────────┐
│                                                            │
│   VirtualNetwork: payments                                 │
│         │                                                  │
│         ▼ generates                                        │
│   ┌─ NetworkPolicy: kube-vnet.mem.platform.payments-… ─┐  │
│   │ select: pods labeled kube-vnet.system/net.…        │  │
│   │ ingress: from same-vnet peers                      │  │
│   └─────────────────────────────────────────────────────┘  │
│                                                            │
│   ┌─ NetworkPolicy: kube-vnet.base ─────────────────────┐  │
│   │ select: all pods   ingress: deny (egress free)     │  │
│   └─────────────────────────────────────────────────────┘  │
│                                                            │
│   pod orders-1 [kube-vnet/net.payments=both] ──┐           │
│                                                ▼ talks     │
│   pod orders-2 [kube-vnet/net.payments=both]               │
│                                                            │
│   pod cron-x   (no label)  ←── isolated by baseline        │
└────────────────────────────────────────────────────────────┘
```

That's the whole core idea. Everything else in this README is variations on it (cross-namespace reach, opt-outs, etc.) or operational details.

## Documentation

Full docs live under [`docs/`](docs/README.md), organized as a tree:

- **New here?** Follow the numbered path: [concepts](docs/getting-started/concepts.md) → [install](docs/getting-started/install.md) → [your first VirtualNetwork](docs/getting-started/first-vnet.md) (hands-on, with a live isolation probe).
- [`docs/guides/`](docs/README.md#guides) — [recipes](docs/guides/recipes.md), [auto-allow](docs/guides/auto-allow.md), [operations](docs/guides/operations.md), [security](docs/guides/security.md), [troubleshooting](docs/guides/troubleshooting.md).
- [`docs/reference/`](docs/README.md#reference) — look-up tables: [CRDs](docs/reference/api.md), [flags & values](docs/reference/configuration.md), [labels & annotations](docs/reference/labels-and-annotations.md), [metrics & events](docs/reference/metrics-and-events.md), [glossary](docs/reference/glossary.md).
- [`docs/internals/`](docs/README.md#internals) — for contributors: [architecture](docs/internals/architecture.md), [source map](docs/internals/code-structure.md), [development](docs/internals/development.md).
- [`docs/faq.md`](docs/faq.md) — cross-cutting Q&A. [`docs/adr/`](docs/adr/README.md) — every accepted decision.

## Prerequisites

- A Kubernetes cluster (1.25+ for the CRD's CEL validation).
- A CNI that **enforces** `NetworkPolicy`: Calico, Cilium, kube-router, Antrea, etc. The operator generates the policies — your CNI is what actually drops packets. Older versions of the default `kindnetd` CNI do not enforce `NetworkPolicy`; check your distribution.

## Quickstart

### 1. Install the operator

Helm (recommended):

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --namespace kube-vnet-system --create-namespace \
  --set operator.clusterBaseline.ingressIsolationLevel=cluster
```

The chart has no default for `operator.clusterBaseline.ingressIsolationLevel`; pick one of `pod`, `namespace`, or `cluster` at install time (`cluster` is the existing-cluster-friendly choice — every pod auto-joins the cluster system vnet, so ingress posture barely changes). See [`docs/install.md`](docs/getting-started/install.md) for the trade-offs.

> Picking `cluster` is the safe adoption default at the cluster level — every pod inherits a `cluster=default-both` membership, so existing traffic keeps flowing. Per-namespace `VirtualNetworkBaseline`s and per-pod labels can opt specific workloads into stricter postures as you migrate. Each example under [`config/samples/`](config/samples/) demonstrates a slice of the baseline-tier model — apply one and run a few `kubectl exec ... curl` probes to see kube-vnet in action.

Or install the rendered manifests directly:

```bash
kubectl apply -f https://github.com/lhns/kube-vnet/releases/download/v0.1.0/release.yaml
```

Or, against the working tree:

```bash
kubectl apply -k config/default
```

Either way you get the `kube-vnet-system` namespace, the `VirtualNetwork` CRD, RBAC, and the controller Deployment.

### 2. Create a network and join pods

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata: { name: payments, namespace: platform }
```

Add the join label to any pod template that should be a member:

```yaml
metadata:
  labels:
    kube-vnet/net.payments: "both"
```

### 3. Inspect

```bash
kubectl get vnet -A
kubectl describe vnet payments -n platform
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet
```

**→ The full hands-on walkthrough — including how to *prove* the isolation works with a live probe — is [`docs/getting-started/first-vnet.md`](docs/getting-started/first-vnet.md).**

## Cross-namespace reach

`spec.allowedNamespaces` controls **which namespaces' pods are allowed to join** — join eligibility, not blanket access; pods there still need the join label. Three union-able matchers (`all: true`, `names: [...]`, `selector: {...}`); the home namespace is always included. Pods outside the home namespace use the prefixed label form with the home namespace baked into the key:

```yaml
# Pod in the VirtualNetwork's home namespace (here: platform):
labels: { kube-vnet/net.payments: "both" }

# Pod in any other namespace (only if allowedNamespaces permits it):
labels: { kube-vnet/net.platform.payments: "both" }
```

Full semantics: [`docs/getting-started/concepts.md § Cross-namespace reach`](docs/getting-started/concepts.md#cross-namespace-reach-allowednamespaces).

## Examples

End-to-end manifests demonstrating each configuration. Each is self-contained — `kubectl apply -f` works on a fresh cluster.

| File | Demonstrates |
|---|---|
| [`config/samples/01_same_namespace.yaml`](config/samples/01_same_namespace.yaml) | Default: only pods in the home namespace can join. |
| [`config/samples/02_two_namespaces.yaml`](config/samples/02_two_namespaces.yaml) | `allowedNamespaces.names: [webapp, monitoring]` — explicit list. |
| [`config/samples/03_label_selector.yaml`](config/samples/03_label_selector.yaml) | `allowedNamespaces.selector: { matchLabels: { tier: prod } }` — label-based. |
| [`config/samples/04_all_namespaces.yaml`](config/samples/04_all_namespaces.yaml) | `allowedNamespaces.all: true` — wildcard, any namespace. |
| [`config/samples/05_disabled_namespace.yaml`](config/samples/05_disabled_namespace.yaml) | Per-namespace opt-out via `kube-vnet/disabled=true`. |
| [`config/samples/06_virtualnetworkbinding.yaml`](config/samples/06_virtualnetworkbinding.yaml) | `VirtualNetworkBinding` — enrolling pods without editing their labels. |
| [`config/samples/08_virtualnetworkbaseline.yaml`](config/samples/08_virtualnetworkbaseline.yaml) | `VirtualNetworkBaseline` — namespace-tier default memberships. |
| [`config/samples/09_clustervirtualnetworkbaseline.yaml`](config/samples/09_clustervirtualnetworkbaseline.yaml) | `ClusterVirtualNetworkBaseline` — the cluster-tier default (what the chart seeds). |

## What the operator does for you

- **Direction modes.** The join label value declares which directions a pod participates in: `both` (default), `ingress`, `egress`, `none`. Asymmetric workloads (a logging sidecar that only sends, a read-only API that only accepts) model their needs directly. See [ADR 0021](docs/adr/0021-direction-modes-on-join-labels.md).
- **`VirtualNetworkBinding` CRD** (short names `vnb`, `vnbs`) — the no-label alternative for enrolling pods you can't modify (third-party Helm charts, pods owned by another operator). A binding lives in the namespace with the pods it selects via a non-empty `podSelector`. See [ADR 0026](docs/adr/0026-virtualnetworkbinding-crd.md).
- **Baselines and inheritance.** The chart seeds a singleton `ClusterVirtualNetworkBaseline` named `default` from `operator.clusterBaseline.ingressIsolationLevel` (one of `pod`/`namespace`/`cluster`). Per-namespace defaults go in a `VirtualNetworkBaseline` (also singleton, named `default`); per-pod overrides go in a `VirtualNetworkBinding` or pod label. Every managed namespace gets a uniform deny-all `NetworkPolicy` named `kube-vnet.base`; vnet membership opens additive ingress allows. Egress is unrestricted by kube-vnet — write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` for per-workload egress restriction. See [ADR 0031](docs/adr/0031-baseline-tier-resolution.md).
- **One or more policies per (vnet, namespace, direction class).** Selectors use an `In` match on the join label value to scope each policy to a single direction class.
- **Auto-allow for externally-exposed Services.** Once the deny-all baseline selects a pod, external traffic (which never matches a `namespaceSelector`) would be blocked — so the operator automatically emits a port-scoped allow (`kube-vnet.ext.svc.<name>-<8hex>`) for every Service of `type: LoadBalancer`, `NodePort`, or `ClusterIP` with `externalIPs`. Ingress controllers and LB-fronted UIs keep working out of the box. Opt out per Service or namespace with `kube-vnet/external-allow=false`. See [ADR 0038](docs/adr/0038-auto-allow-externally-exposed-services.md).
- **Auto-allow for `hostPort` pods.** The same treatment for pods bound directly to node ports: one policy per `(namespace, port, protocol)` (`kube-vnet.ext.host.<port>.<proto>-<8hex>`). See [ADR 0040](docs/adr/0040-auto-allow-hostport-pods.md).
- **Auto-allow for Services the apiserver calls.** Admission webhooks (cert-manager, kyverno, gatekeeper, …), aggregated APIServices (metrics-server, …), and CRD conversion webhooks are dialed by the kube-apiserver — which isn't a pod, so no selector-based rule can admit it. The operator watches the four discovery resources that declare these backends and emits `kube-vnet.ext.apiserver.<name>-<8hex>` allows on the referenced targetPorts; admission keeps working without disabling kube-vnet for those namespaces. The allowed source CIDR is configurable via `operator.apiserverSourceCIDR` (default `0.0.0.0/0`, matching the no-NetworkPolicy baseline). See [ADR 0041](docs/adr/0041-auto-allow-apiserver-reachable-services.md).
- **Drift correction.** If someone edits an operator-managed `NetworkPolicy` by hand, the next reconcile reverts it.
- **Clean deletion.** Deleting a VirtualNetwork removes every policy it generated, including across namespaces.
- **Status & events.** Each VirtualNetwork carries `Ready` and `Degraded` conditions; transitions emit Kubernetes Events visible in `kubectl describe` and event aggregators. The full reason taxonomy is in [ADR 0012](docs/adr/0012-status-conditions-ready-and-degraded.md).

## Disabling the operator for a namespace

Two equivalent ways:

- **Per-namespace** — annotate the namespace:

  ```yaml
  metadata:
    annotations:
      kube-vnet/disabled: "true"
  ```

- **Operator-wide** — pass `--disabled-namespaces=foo,bar` to the controller (Helm: `operator.disabledNamespaces`). Default: `[kube-system, kube-public, kube-node-lease]`. The operator's own namespace is always added implicitly. Disabled namespaces get no kube-vnet objects of any kind: no baseline, no system vnets, no membership policies, no resolution stamping.

When a namespace is unmanaged: no baseline is created, no membership policies are generated for pods in that namespace, and pods in that namespace are not eligible joiners for any VirtualNetwork (regardless of `allowedNamespaces`).

## Ingress posture (cluster-wide via baselines)

Every managed namespace gets the deny-all ingress baseline `kube-vnet.base`; how much of it bites is set by a three-tier inheritance chain — cluster baseline → namespace baseline → pod tier (bindings + labels) — where `default-*` direction values are overridable by lower tiers and bare values are enforced. The chart's required `operator.clusterBaseline.ingressIsolationLevel` preset seeds the cluster tier:

| Level | Seeded cluster baseline |
|---|---|
| `pod` | `namespace=default-egress, cluster=default-egress` — strict; ingress only via explicit membership |
| `namespace` | `namespace=default-both, cluster=default-egress` — same-NS reachable, cross-NS egress only |
| `cluster` | `namespace=default-both, cluster=default-both` — no isolation (safe adoption default) |

Full model: [`docs/getting-started/concepts.md § The deny-all baseline`](docs/getting-started/concepts.md#the-deny-all-baseline) and [ADR 0031](docs/adr/0031-baseline-tier-resolution.md).

## Configuration

The values you'll actually set (everything else has sensible defaults):

| Helm value | Default | Why you'd set it |
|---|---|---|
| `operator.clusterBaseline.ingressIsolationLevel` | *(none — required)* | The isolation decision: `pod` / `namespace` / `cluster`. |
| `operator.disabledNamespaces` | `[kube-system, kube-public, kube-node-lease]` | Namespaces the operator never touches. |
| `operator.apiserverSourceCIDR` | `0.0.0.0/0` | Narrow the auto-allow for apiserver-dialed webhooks to your control-plane subnet. |
| `replicaCount` | `1` | `2` for HA (leader election is already on). |

Every flag, value, and env var: [`docs/reference/configuration.md`](docs/reference/configuration.md).

## Observability

Six domain metrics on `:8080/metrics` (reconcile outcomes/latency, vnet/policy/member counts, apply errors), plus status conditions and Kubernetes Events on every transition. The full surface with sample Prometheus alert rules: [`docs/reference/metrics-and-events.md`](docs/reference/metrics-and-events.md).

## Project layout

```
api/v1alpha1/         # CRD Go types
cmd/main.go           # operator entrypoint
internal/controller/  # reconciler, policy generator, baseline, namespace filter
config/               # CRD, RBAC, Deployment manifests (kustomize)
config/samples/       # runnable example VirtualNetworks
docs/                 # documentation tree (getting-started/, guides/, reference/, internals/, adr/)
test/e2e/             # kind+CNI traffic tests
```

## Development & testing

```bash
make manifests           # regenerate CRD + RBAC
make generate            # regenerate deepcopy
make test                # unit tests (sub-second)
make integration-test    # envtest-backed integration suite (~10s; needs Go only)
make e2e                 # kind end-to-end (needs Docker). Default CNI: kube-router.
                         #   override with: ./test/e2e/up.sh calico
make build               # build the binary into bin/manager
make docker-build IMG=…  # build the container image
```

The three test rungs (unit, integration, e2e against kube-router and Calico) and their CI lanes are described in [ADR 0018](docs/adr/0018-test-strategy-envtest-and-kind-calico.md).

## Architecture decisions

Significant design and implementation choices are recorded as ADRs in [`docs/adr/`](docs/adr/README.md). The longer-form rationale lives in [`docs/internals/design.md`](docs/internals/design.md) (historical); where the design doc and the ADRs disagree (the doc was written first), the ADRs are the source of truth.

## Status

`v1alpha1`. Single-cluster only. Generates plain `networking.k8s.io/v1` `NetworkPolicy`. The remaining gap to v1-complete (a label-cardinality stress test) is tracked in [ADR 0014](docs/adr/0014-deferred-v1-items.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
