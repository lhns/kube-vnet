# kube-vnet

A Kubernetes operator that introduces a `VirtualNetwork` custom resource — a Docker-Swarm-style named-network primitive — and translates VirtualNetwork membership into standard `NetworkPolicy` resources.

> See [`kube-vnet-design.md`](./kube-vnet-design.md) for the design rationale and full specification.

## TL;DR

- Define a `VirtualNetwork`. Pods join it with one label per network. Pods in the same VirtualNetwork can talk; pods in different (or no) VirtualNetworks are isolated by an automatic default-deny baseline.
- The output is plain `networking.k8s.io/v1` `NetworkPolicy` — no CNI extensions required, no lock-in.

## Quickstart

### 1. Install the operator

```bash
kubectl apply -k config/default
```

This creates the `kube-vnet-system` namespace, the CRD, RBAC, and the controller Deployment.

### 2. Define a VirtualNetwork

```yaml
apiVersion: kube-vnet/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: platform
spec:
  extent: Namespace      # Namespace (default) or Cluster
```

### 3. Have pods join it

Add the join label to your Deployment's pod template:

```yaml
metadata:
  labels:
    kube-vnet/net.payments: "true"
```

For a pod in a *different* namespace joining a `Cluster`-extent VirtualNetwork, use the namespace-prefixed form:

```yaml
metadata:
  labels:
    kube-vnet/net.<vnet-namespace>.<vnet-name>: "true"
```

### 4. Inspect

```bash
kubectl get vnet -A
kubectl describe vnet payments -n platform
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet
```

## Behavior

- **Default-deny baseline.** In any managed namespace where at least one pod joins a VirtualNetwork, the operator ensures a `NetworkPolicy` named `kube-vnet-default-deny` exists. It allows egress to CoreDNS only; everything else is denied unless an additional policy permits it.
- **Per-VirtualNetwork policies.** For each VirtualNetwork with members, one `NetworkPolicy` is generated per namespace that has members. The selector is a single `Exists` match on the join label key.
- **Cluster extent.** If `spec.extent: Cluster`, pods in any namespace can join via the namespace-prefixed label. The operator generates policies in each namespace that has members and references peers cross-namespace via `namespaceSelector + podSelector`.
- **Drift correction.** Edits to operator-managed `NetworkPolicy` resources are reverted on the next reconcile.
- **Cleanup.** Deleting a VirtualNetwork removes all generated policies, including those in other namespaces.

## Disabling the operator for a namespace

Two ways, both equivalent:

- **Per-namespace** — annotate the namespace:

  ```yaml
  metadata:
    annotations:
      kube-vnet/disabled: "true"
  ```

- **Operator-wide** — pass `--excluded-namespaces=foo,bar` to the controller. Defaults: `kube-system,kube-public,kube-node-lease`. The operator's own namespace is always added.

When a namespace is unmanaged: no baseline is created, no membership policies are generated for pods in that namespace, and pods in that namespace are not eligible peers for `Cluster`-extent VirtualNetworks defined elsewhere.

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:8081` | health/readiness endpoint |
| `--leader-elect` | `false` | enable leader election (turn on for HA) |
| `--label-prefix` | `kube-vnet/` | prefix for the join label keys |
| `--excluded-namespaces` | `kube-system,kube-public,kube-node-lease` | comma-separated namespaces excluded from kube-vnet management |

## Development

```bash
make manifests       # regenerate CRD + RBAC
make generate        # regenerate deepcopy
make test            # unit tests
make build           # build the binary into bin/manager
make docker-build IMG=...    # build the container image
```

## Status

v1alpha1. The CRD is named `VirtualNetwork` (`vnet`, `vnets`) under `kube-vnet/v1alpha1`. Single-cluster only. Generates plain `networking.k8s.io/v1` `NetworkPolicy`. See the design doc for what's deferred to future versions (Fleet extent, CNI-specific output, L7/DNS/identity policy, etc.).

## License

TBD.
