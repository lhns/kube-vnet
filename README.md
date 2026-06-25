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

- `kube-vnet.payments-<8hex>` — a `NetworkPolicy` selecting any pod with the operator-stamped `kube-vnet.system/net.payments` label, allowing ingress from peers in the same vnet (filtered by their declared direction).
- `kube-vnet` — a uniform deny-all ingress baseline, installed in every managed namespace, so non-members aren't reachable. Egress is unrestricted by kube-vnet — for per-workload egress restriction, add a user-managed `NetworkPolicy` with `policyTypes: [Egress]`.

The picture:

```
┌─ namespace: platform ──────────────────────────────────┐
│                                                        │
│   VirtualNetwork: payments                             │
│         │                                              │
│         ▼ generates                                    │
│   ┌─ NetworkPolicy: kube-vnet.payments-<8hex> ─────┐  │
│   │ select: pods labeled kube-vnet.system/net.…    │  │
│   │ ingress: from same-vnet peers                   │  │
│   └─────────────────────────────────────────────────┘  │
│                                                        │
│   ┌─ NetworkPolicy: kube-vnet ──────────────────────┐  │
│   │ select: all pods   ingress: deny (egress free)  │  │
│   └─────────────────────────────────────────────────┘  │
│                                                        │
│   pod orders-1 [kube-vnet/net.payments=both] ──┐       │
│                                                ▼ talks │
│   pod orders-2 [kube-vnet/net.payments=both]           │
│                                                        │
│   pod cron-x   (no label)  ←── isolated by baseline    │
└────────────────────────────────────────────────────────┘
```

That's the whole core idea. Everything else in this README is variations on it (cross-namespace reach, opt-outs, etc.) or operational details.

## Documentation

Full docs live under [`docs/`](docs/README.md):

- **New here** → [`docs/concepts.md`](docs/concepts.md) (the model in depth) and [`docs/faq.md`](docs/faq.md).
- **Installing** → [`docs/install.md`](docs/install.md) (Helm, kubectl, signature verification).
- **Day-to-day usage** → [`docs/recipes.md`](docs/recipes.md) (worked examples) and [`docs/reference/`](docs/reference/) (look-up tables).
- **Running it in production** → [`docs/operations.md`](docs/operations.md) and [`docs/security.md`](docs/security.md).
- **Something is broken** → [`docs/troubleshooting.md`](docs/troubleshooting.md).
- **Contributing** → [`docs/development.md`](docs/development.md), [`docs/architecture.md`](docs/architecture.md), and the [ADRs](docs/adr/README.md).

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

The chart has no default for `operator.clusterBaseline.ingressIsolationLevel`; pick one of `pod`, `namespace`, or `cluster` at install time (`cluster` is the existing-cluster-friendly choice — every pod auto-joins the cluster system vnet, so ingress posture barely changes). See [`docs/install.md`](docs/install.md) for the trade-offs.

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

### 2. Define a VirtualNetwork

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
```

### 3. Label pods that should join

In your Deployment's pod template:

```yaml
metadata:
  labels:
    kube-vnet/net.payments: "both"
```

### 4. Inspect

```bash
kubectl get vnet -A
kubectl describe vnet payments -n platform
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet
```

## Cross-namespace reach

`spec.allowedNamespaces` controls **which namespaces' pods are allowed to join** the network — not which pods are blanket-granted access. A pod in an allowed namespace still needs to opt in by adding the prefixed join label; pods in those namespaces that *don't* carry the label get nothing.

By default (field omitted), only pods in the home namespace can join. To let pods from other namespaces in:

```yaml
spec:
  allowedNamespaces:
    names: [webapp, monitoring]   # explicit list
```

Three matchers are supported, and they union:

| Field | Meaning |
|---|---|
| `all: true` | Pods in any namespace may join (when they add the join label). |
| `names: [a, b]` | Pods in these namespaces may join (when they add the join label). Names match exactly — no glob/regex; use `selector` for groups. |
| `selector: { matchLabels: { tier: prod } }` | Pods in namespaces matching the label selector may join (when they add the join label). |

The home namespace is always allowed implicitly. Glob patterns are deliberately not supported — use `selector` for groups.

### Two label forms

A pod's join label depends on whether it lives in the home namespace or another one:

```yaml
# Pod in the VirtualNetwork's home namespace (here: platform):
labels: { kube-vnet/net.payments: "both" }

# Pod in any other namespace (only if allowedNamespaces permits it):
labels: { kube-vnet/net.platform.payments: "both" }
#                       ^^^^^^^^ home namespace baked into the label key
```

VirtualNetwork names cannot contain dots — the apiserver rejects names that aren't DNS-1123 labels (no dots, lowercase alphanumeric and hyphens) via a CRD validation rule.

## Examples

End-to-end manifests demonstrating each configuration. Each is self-contained — `kubectl apply -f` works on a fresh cluster.

| File | Demonstrates |
|---|---|
| [`config/samples/01_same_namespace.yaml`](config/samples/01_same_namespace.yaml) | Default: only pods in the home namespace can join. |
| [`config/samples/02_two_namespaces.yaml`](config/samples/02_two_namespaces.yaml) | `allowedNamespaces.names: [webapp, monitoring]` — explicit list. |
| [`config/samples/03_label_selector.yaml`](config/samples/03_label_selector.yaml) | `allowedNamespaces.selector: { matchLabels: { tier: prod } }` — label-based. |
| [`config/samples/04_all_namespaces.yaml`](config/samples/04_all_namespaces.yaml) | `allowedNamespaces.all: true` — wildcard, any namespace. |
| [`config/samples/05_disabled_namespace.yaml`](config/samples/05_disabled_namespace.yaml) | Per-namespace opt-out via `kube-vnet/disabled=true`. |

## What the operator does for you

- **Direction modes.** The join label value declares which directions a pod participates in: `both` (default), `ingress`, `egress`, `none`. Asymmetric workloads (a logging sidecar that only sends, a read-only API that only accepts) model their needs directly. See [ADR 0021](docs/adr/0021-direction-modes-on-join-labels.md).
- **`VirtualNetworkBinding` CRD** (short names `vnb`, `vnbs`) — the no-label alternative for enrolling pods you can't modify (third-party Helm charts, pods owned by another operator). A binding lives in the namespace with the pods it selects via a non-empty `podSelector`. See [ADR 0026](docs/adr/0026-virtualnetworkbinding-crd.md).
- **Baselines and inheritance.** The chart seeds a singleton `ClusterVirtualNetworkBaseline` named `default` from `operator.clusterBaseline.ingressIsolationLevel` (one of `pod`/`namespace`/`cluster`). Per-namespace defaults go in a `VirtualNetworkBaseline` (also singleton, named `default`); per-pod overrides go in a `VirtualNetworkBinding` or pod label. Every managed namespace gets a uniform deny-all `NetworkPolicy` named `kube-vnet`; vnet membership opens additive ingress allows. Egress is unrestricted by kube-vnet — write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` for per-workload egress restriction. See [ADR 0031](docs/adr/0031-baseline-tier-resolution.md).
- **One or more policies per (vnet, namespace, direction class).** Selectors use an `In` match on the join label value to scope each policy to a single direction class.
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

The baseline shape is uniform: every managed namespace gets a deny-all ingress `NetworkPolicy` named `kube-vnet`. Pods open ingress allows by joining vnets — driven by inheritance through three tiers:

| Tier | Resource | Authority |
|---|---|---|
| Cluster baseline | `ClusterVirtualNetworkBaseline` (singleton named `default`; chart-seeded from `operator.clusterBaseline.ingressIsolationLevel`) | cluster-admin |
| Namespace baseline | `VirtualNetworkBaseline` (singleton per namespace named `default`) | namespace-admin |
| Pod tier | `VirtualNetworkBinding` (must select specific pods) + the `kube-vnet/net.<vnet>=<dir>` pod label | namespace-admin / pod author |

Direction values are the bare four (`both`, `ingress`, `egress`, `none`) at the pod tier; baselines also accept four `default-*` variants that mark the value as override-permitted by lower tiers (bare = enforced). Conflicts at the pod tier (binding vs label, or two bindings) intersect — fail-closed.

The chart's `operator.clusterBaseline.ingressIsolationLevel` preset maps to:

| Level | Seeded cluster baseline |
|---|---|
| `pod` | `namespace=default-egress, cluster=default-egress` — strict; ingress only via explicit binding/label |
| `namespace` | `namespace=default-both, cluster=default-egress` — same-NS reachable, cross-NS egress only |
| `cluster` | `namespace=default-both, cluster=default-both` — no isolation (allow-all) |

For more, see [`docs/concepts.md`](docs/concepts.md), [`docs/install.md`](docs/install.md), and [ADR 0031](docs/adr/0031-baseline-tier-resolution.md).

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:8081` | health/readiness endpoint |
| `--leader-elect` | `false` | enable leader election (turn on for HA) |
| `--disabled-namespaces` | `kube-system,kube-public,kube-node-lease` | comma-separated namespaces the operator never touches (mirrors `kube-vnet/disabled=true`) |

Baseline contents come from the `ClusterVirtualNetworkBaseline` CR, configured in the chart via `operator.clusterBaseline.{create, ingressIsolationLevel, memberships}`. See [`docs/reference/configuration.md`](docs/reference/configuration.md).

## Observability

The operator exposes the controller-runtime defaults plus six domain-specific metrics on `:8080/metrics`:

| Metric | Type | Description |
|---|---|---|
| `kube_vnet_reconciliations_total{result}` | counter | Reconcile outcomes (`success`/`error`) |
| `kube_vnet_reconcile_duration_seconds` | histogram | Reconcile latency |
| `kube_vnet_networks_total` | gauge | VirtualNetwork resources observed |
| `kube_vnet_managed_policies_total` | gauge | NetworkPolicies managed by the operator |
| `kube_vnet_members_total{network}` | gauge | Members per VirtualNetwork |
| `kube_vnet_apply_errors_total{kind}` | counter | Apply errors (`membership_policy`/`baseline`) |

Status conditions and Events on transitions complement the metrics — see [ADR 0012](docs/adr/0012-status-conditions-ready-and-degraded.md) and [ADR 0016](docs/adr/0016-emit-events-on-condition-transitions.md).

## Project layout

```
api/v1alpha1/         # CRD Go types
cmd/main.go           # operator entrypoint
internal/controller/  # reconciler, policy generator, baseline, namespace filter
config/               # CRD, RBAC, Deployment manifests (kustomize)
config/samples/       # runnable example VirtualNetworks
docs/                 # design doc + ADRs
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

Significant design and implementation choices are recorded as ADRs in [`docs/adr/`](docs/adr/README.md). The longer-form rationale lives in [`docs/kube-vnet-design.md`](docs/kube-vnet-design.md); where the design doc and the ADRs disagree (the doc was written first), the ADRs are the source of truth.

## Status

`v1alpha1`. Single-cluster only. Generates plain `networking.k8s.io/v1` `NetworkPolicy`. The remaining gap to v1-complete (a label-cardinality stress test) is tracked in [ADR 0014](docs/adr/0014-deferred-v1-items.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
