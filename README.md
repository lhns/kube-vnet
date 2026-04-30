# kube-vnet

A Kubernetes operator that introduces a `VirtualNetwork` custom resource — a Docker-Swarm-style named-network primitive — and translates VirtualNetwork membership into standard `NetworkPolicy` resources.

> Background: [`docs/kube-vnet-design.md`](./docs/kube-vnet-design.md) — the original design doc.
> Decisions: [`docs/adr/`](./docs/adr/README.md) — Architecture Decision Records, the source of truth for what's actually implemented (some details supersede the design doc).

## TL;DR

- Define a `VirtualNetwork`. Pods join it with one label per network. Pods on the same VirtualNetwork can talk; pods on different (or no) VirtualNetworks are isolated by an automatic default-deny baseline.
- Reach is controlled by `spec.allowedNamespaces` — by default only the home namespace; can list namespaces, match by label, or use a wildcard.
- The output is plain `networking.k8s.io/v1` `NetworkPolicy` — no CNI extensions required, no lock-in.

## Quickstart

### 1. Install the operator

```bash
kubectl apply -k config/default
```

This creates the `kube-vnet-system` namespace, the CRD, RBAC, and the controller Deployment.

### 2. Define a VirtualNetwork

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
spec:
  # Default: only pods in the home namespace (platform) can join.
  # Optional: open it up — see config/samples/*.yaml for every form.
  allowedNamespaces:
    names: [webapp, monitoring]
```

### 3. Have pods join it

Add the join label to the Deployment's pod template:

```yaml
metadata:
  labels:
    # Bare form — for pods in the VirtualNetwork's home namespace.
    kube-vnet/net.payments: "true"
```

For a pod in a *different* namespace joining a VirtualNetwork that allows it, use the namespace-prefixed form:

```yaml
metadata:
  labels:
    # Prefixed form — for pods in any other namespace.
    kube-vnet/net.<vnet-namespace>.<vnet-name>: "true"
```

### 4. Inspect

```bash
kubectl get vnet -A
kubectl describe vnet payments -n platform
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet
```

## Samples

End-to-end manifests demonstrating each configuration. Each is self-contained — `kubectl apply -f` works on a fresh cluster.

| File | Demonstrates |
|---|---|
| [`config/samples/01_same_namespace.yaml`](config/samples/01_same_namespace.yaml) | Default: only pods in the home namespace can join. |
| [`config/samples/02_two_namespaces.yaml`](config/samples/02_two_namespaces.yaml) | `allowedNamespaces.names: [webapp, monitoring]` — explicit list. |
| [`config/samples/03_label_selector.yaml`](config/samples/03_label_selector.yaml) | `allowedNamespaces.selector: { matchLabels: { tier: prod } }` — label-based. |
| [`config/samples/04_all_namespaces.yaml`](config/samples/04_all_namespaces.yaml) | `allowedNamespaces.all: true` — wildcard, any namespace. |
| [`config/samples/05_disabled_namespace.yaml`](config/samples/05_disabled_namespace.yaml) | Per-namespace opt-out via `kube-vnet/disabled=true`. |

## Behavior

- **Default-deny baseline.** In any managed namespace where at least one pod joins a VirtualNetwork, the operator ensures `kube-vnet-default-deny` exists. It allows egress to CoreDNS only; everything else is denied unless an additional policy permits it.
- **Per-VirtualNetwork policies.** For each VirtualNetwork with members, one `NetworkPolicy` is generated per namespace that has members. The selector is a single `Exists` match on the join label key.
- **Cross-namespace reach.** Controlled by `spec.allowedNamespaces`. Pods in non-permitted namespaces that nonetheless carry the join label are surfaced as `Degraded` reason `InvalidJoiners`.
- **Drift correction.** Edits to operator-managed `NetworkPolicy` resources are reverted on the next reconcile.
- **Cleanup.** Deleting a VirtualNetwork removes all generated policies, including those in other namespaces.
- **Status & events.** `Ready`/`Degraded` conditions surface state; transitions emit Kubernetes Events visible in `kubectl describe`. See [ADR 0012](docs/adr/0012-status-conditions-ready-and-degraded.md) for the full reason taxonomy.

## Disabling the operator for a namespace

Two ways, both equivalent:

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

## Architecture decisions

The implementation's significant decisions are recorded as ADRs in [`docs/adr/`](docs/adr/README.md). Highlights:

- [0005 — Namespaced CRD with `allowedNamespaces`](docs/adr/0005-namespaced-crd-with-allowed-namespaces.md) (supersedes the design doc's `spec.extent`)
- [0006 — Single per-namespace opt-out via `kube-vnet/disabled`](docs/adr/0006-baseline-default-deny-and-single-opt-out.md)
- [0009 — Server-side apply with field manager](docs/adr/0009-server-side-apply-with-field-manager.md)
- [0013 — Pod watch with `handler.Funcs` for removals](docs/adr/0013-pod-watch-with-handler-funcs-for-removals.md)
- [0018 — Test strategy: unit + envtest + kind+Calico](docs/adr/0018-test-strategy-envtest-and-kind-calico.md)
- [0014 — Deferred v1 items](docs/adr/0014-deferred-v1-items.md) (only the label-cardinality stress test remains)

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

Status conditions (`Ready`, `Degraded`) and Kubernetes Events on transitions provide per-resource visibility. See [ADR 0012](docs/adr/0012-status-conditions-ready-and-degraded.md) and [ADR 0016](docs/adr/0016-emit-events-on-condition-transitions.md).

## Development

```bash
make manifests           # regenerate CRD + RBAC
make generate            # regenerate deepcopy
make test                # unit tests (sub-second)
make integration-test    # envtest-backed integration suite (~10s; requires Go)
make e2e                 # kind end-to-end (requires Docker). Default CNI: kube-router.
                         #   override with: ./hack/e2e-up.sh calico
make build               # build the binary into bin/manager
make docker-build IMG=…  # build the container image
```

The three test rungs (unit, integration, e2e) and their CI lanes are documented in [ADR 0018](docs/adr/0018-test-strategy-envtest-and-kind-calico.md).

## Status

`v1alpha1`. Single-cluster only. Generates plain `networking.k8s.io/v1` `NetworkPolicy`. See [ADR 0014](docs/adr/0014-deferred-v1-items.md) for the remaining gap to v1-complete (a label-cardinality stress test).

## License

TBD.
