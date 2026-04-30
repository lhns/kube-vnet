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
        kube-vnet/net.payments: "true"   # ← join the payments network
    spec:
      containers: [{ name: app, image: nginx:alpine }]
```

The operator notices and produces, in the `platform` namespace:

- `kube-vnet-payments-platform` — a `NetworkPolicy` selecting any pod with the `kube-vnet/net.payments` label, allowing ingress from and egress to peers carrying the same label, plus DNS to CoreDNS.
- `kube-vnet-default-deny` — a baseline policy denying everything that isn't explicitly allowed, so pods *not* on the network can't reach pods that are.

The picture:

```
┌─ namespace: platform ──────────────────────────────────┐
│                                                        │
│   VirtualNetwork: payments                             │
│         │                                              │
│         ▼ generates                                    │
│   ┌─ NetworkPolicy: kube-vnet-payments-platform ────┐  │
│   │ select: pods labeled kube-vnet/net.payments     │  │
│   │ ingress/egress: from/to same label + DNS        │  │
│   └─────────────────────────────────────────────────┘  │
│                                                        │
│   ┌─ NetworkPolicy: kube-vnet-default-deny ─────────┐  │
│   │ select: all pods   ingress/egress: deny + DNS   │  │
│   └─────────────────────────────────────────────────┘  │
│                                                        │
│   pod orders-1 [kube-vnet/net.payments=true] ──┐       │
│                                                ▼ talks │
│   pod orders-2 [kube-vnet/net.payments=true]           │
│                                                        │
│   pod cron-x   (no label)  ←── isolated by baseline    │
└────────────────────────────────────────────────────────┘
```

That's the whole core idea. Everything else in this README is variations on it (cross-namespace reach, opt-outs, etc.) or operational details.

## Prerequisites

- A Kubernetes cluster (1.25+ for the CRD's CEL validation).
- A CNI that **enforces** `NetworkPolicy`: Calico, Cilium, kube-router, Antrea, etc. The operator generates the policies — your CNI is what actually drops packets. Older versions of the default `kindnetd` CNI do not enforce `NetworkPolicy`; check your distribution.

## Quickstart

### 1. Install the operator

```bash
kubectl apply -k config/default
```

This creates the `kube-vnet-system` namespace, the `VirtualNetwork` CRD, RBAC, and the controller Deployment.

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
    kube-vnet/net.payments: "true"
```

### 4. Inspect

```bash
kubectl get vnet -A
kubectl describe vnet payments -n platform
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet
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
labels: { kube-vnet/net.payments: "true" }

# Pod in any other namespace (only if allowedNamespaces permits it):
labels: { kube-vnet/net.platform.payments: "true" }
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

- **Default-deny baseline.** In any managed namespace where at least one pod joins a VirtualNetwork, `kube-vnet-default-deny` is installed. It denies everything except DNS to CoreDNS, so unjoined pods are actually isolated. The baseline is removed automatically when the last member leaves.
- **One policy per (vnet, namespace).** For each VirtualNetwork with members, one `NetworkPolicy` is generated per namespace that has members. Selectors are a single `Exists` match on the join label key — easy to read at a glance.
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

- **Operator-wide** — pass `--excluded-namespaces=foo,bar` to the controller. Defaults: `kube-system,kube-public,kube-node-lease`. The operator's own namespace is always added.

When a namespace is unmanaged: no baseline is created, no membership policies are generated for pods in that namespace, and pods in that namespace are not eligible joiners for any VirtualNetwork (regardless of `allowedNamespaces`).

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:8081` | health/readiness endpoint |
| `--leader-elect` | `false` | enable leader election (turn on for HA) |
| `--label-prefix` | `kube-vnet/` | prefix for the join label keys |
| `--excluded-namespaces` | `kube-system,kube-public,kube-node-lease` | comma-separated namespaces excluded from kube-vnet management |

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
                         #   override with: ./hack/e2e-up.sh calico
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
