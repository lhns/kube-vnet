# kube-vnet

A Kubernetes operator that lets you declare **named networks** as a first-class resource. Services join a network with a label; the operator generates the underlying `NetworkPolicy` resources so only same-network pods can talk to each other. The output is plain `networking.k8s.io/v1`: no CNI extensions, no lock-in. Uninstall the operator and the generated policies keep working.

## The idea

Raw `NetworkPolicy` is exception-based and selector-based; teams think in memberships: *"payments joins the payments network, so does orders; nothing else reaches them."* kube-vnet gives you that model (Docker Swarm's named networks, on stock Kubernetes):

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata: { name: payments, namespace: platform }
```

```yaml
# in any pod template that should be a member:
metadata:
  labels:
    kube-vnet/net.payments: "both"
```

From this the operator maintains, in every managed namespace, a deny-all ingress baseline (`kube-vnet.base`) and, per network, a membership policy (`kube-vnet.mem.platform.payments-<8hex>`) that lets members reach each other. Non-members are isolated; egress is never restricted. Full model with diagrams: [`docs/getting-started/concepts.md`](docs/getting-started/concepts.md).

---

## Running it: the cheat sheet

Everything you need to operate kube-vnet, on one screen. Details behind each link.

### Install

Kubernetes ≥ 1.25 and a CNI that **enforces** NetworkPolicy (Calico, Cilium, kube-router, Antrea, managed-cloud variants; [full list](docs/getting-started/install.md#prerequisites)).

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --namespace kube-vnet-system --create-namespace \
  --set operator.clusterBaseline.ingressIsolationLevel=cluster   # ← required choice, see below
```

No Helm: `kubectl apply -f https://github.com/lhns/kube-vnet/releases/download/v0.1.0/release.yaml` or `kubectl apply -k config/default` ([other paths, air-gapped, signatures](docs/getting-started/install.md)).

### The one required choice: isolation level

| `ingressIsolationLevel` | Effect | Seeded cluster baseline |
|---|---|---|
| `cluster` | No isolation yet; the **safe adoption default**. Traffic flows as before, tighten later per namespace | `namespace=default-both, cluster=default-both` |
| `namespace` | Same-namespace pods reach each other; cross-namespace only via membership | `namespace=default-both, cluster=default-egress` |
| `pod` | Strict: ingress only via explicit membership | `namespace=default-egress, cluster=default-egress` |

`default-*` values are overridable per namespace/pod, so any preset can be tightened or loosened locally ([how](docs/getting-started/concepts.md#the-deny-all-baseline)).

### Values you'll actually set

| Helm value | Default | Purpose |
|---|---|---|
| `operator.clusterBaseline.ingressIsolationLevel` | *(none; required)* | The isolation choice above |
| `operator.disabledNamespaces` | `[kube-system]` | Namespaces the operator never touches (`[]` disables none; remove `kube-system` to enroll it — DNS stays up via `dnsCarveout`) |
| `operator.apiserverSourceCIDR` | `0.0.0.0/0` | Narrow the webhook auto-allow to your control-plane subnet |
| `replicaCount` | `1` | `2` for HA (leader election already on) |

Everything else: [`docs/reference/configuration.md`](docs/reference/configuration.md).

### The CRDs

| Kind | Short | Scope | Purpose |
|---|---|---|---|
| `VirtualNetwork` | `vnet` | namespaced | A named network; `spec.allowedNamespaces` controls who may join |
| `VirtualNetworkBinding` | `vnb` | namespaced | Join pods **without editing their labels** (third-party charts) |
| `VirtualNetworkBaseline` | `vnbl` | namespaced, singleton `default` | Namespace-wide default memberships |
| `ClusterVirtualNetworkBaseline` | `cvnbl` | cluster, singleton `default` | Cluster-wide defaults (what the chart seeds) |

Two system vnets exist automatically: `namespace` (per-NS, = "reachable by my namespace") and `cluster` (= "reachable cluster-wide"). Field-level reference: [`docs/reference/api.md`](docs/reference/api.md).

### Three ways to join a network

| Mechanism | How | Use when |
|---|---|---|
| Pod label | `kube-vnet/net.<vnet>: both` (vnet in own NS) or `kube-vnet/net.<homeNS>.<vnet>: both` (elsewhere) | You own the pod template |
| `VirtualNetworkBinding` | `spec: { virtualNetworkRef: {name, namespace}, direction: both, podSelector: {...} }` in the pods' namespace | You can't touch the template |
| Baselines | `memberships:` list on `vnbl`/`cvnbl` | Defaults every pod inherits |

### Directions

| Value | Meaning | Legal on |
|---|---|---|
| `both` | accept from + initiate to members | label, binding, baseline |
| `ingress` | accept only | label, binding, baseline |
| `egress` | initiate only | label, binding, baseline |
| `none` | not a member (cancels inherited) | label, binding, baseline |
| `default-both` / `-ingress` / `-egress` / `-none` | same, but overridable by lower tiers | baselines only |

Resolution is fail-closed: within a tier, conflicting directions intersect; across tiers, bare values are enforced, `default-*` may be overridden ([algebra](docs/getting-started/concepts.md#direction-modes-on-the-join-label)).

### Cross-namespace

```yaml
spec:
  allowedNamespaces:        # join ELIGIBILITY; pods still need a membership
    all: true               # any namespace, OR
    names: [webapp, mon]    # explicit list, OR
    selector: { matchLabels: { tier: prod } }   # by NS label (matchers union)
```

Home namespace is always included. Foreign pods use the prefixed label form (`kube-vnet/net.<homeNS>.<vnet>`).

### Annotations & opt-outs

| Annotation | On | Effect |
|---|---|---|
| `kube-vnet/disabled: "true"` | Namespace | Operator does nothing here (no baseline, no policies, pods not joinable) |
| `kube-vnet/external-allow: "false"` | Service or Namespace | Opt out of all auto-allow families |
| `kube-vnet/apiserver-reachable: "true"` | Service | Opt IN to the apiserver auto-allow when no webhook/APIService declares it |

### The NetworkPolicies the operator creates

These are `NetworkPolicy` objects. Every managed namespace contains some subset of:

```
NAME                                       WHAT IT IS
kube-vnet.base                             deny-all ingress baseline (per managed NS)
kube-vnet.mem.<homeNS>.<vnet>-<8hex>       membership policy per (vnet, member NS)
kube-vnet.ext.svc.<service>-<8hex>         auto-allow: LoadBalancer/NodePort Service
kube-vnet.ext.host.<port>.<proto>-<8hex>   auto-allow: hostPort pod
kube-vnet.ext.apiserver.<service>-<8hex>   auto-allow: Service the apiserver dials
```

Hand-edits to any of these are reverted (drift correction); deleting a vnet removes every policy it generated. The operator also stamps confirmed memberships onto pods as `kube-vnet.system/net.<homeNS>.<vnet>=<direction>` labels. Your `kube-vnet/…` label is the request; the operator-owned `kube-vnet.system/…` stamp is what the policies actually select on ([full label contract](docs/reference/labels-and-annotations.md)).

### Allowed automatically

So the deny-all baseline doesn't break traffic whose source no pod selector can match ([full guide](docs/guides/auto-allow.md)):

- **Externally-exposed Services** (LoadBalancer / NodePort / externalIPs): your ingress controller keeps working.
- **hostPort pods**: node-bound daemons stay reachable.
- **Services the apiserver dials**: admission webhooks (cert-manager, kyverno, …) and metrics-server keep answering.
- **Cluster DNS**, if you enroll `kube-system`: a chart-shipped `NetworkPolicy` (`kube-vnet-coredns-allow`, distinct from the operator's own policies above) keeps CoreDNS reachable on `:53`. Configured via `dnsCarveout.*`; see [ADR 0042](docs/adr/0042-coredns-ingress-carveout-and-kube-system-enrollment.md).

### Inspect

```bash
kubectl get vnet -A                                              # networks + Ready status
kubectl describe vnet payments -n platform                       # members, policies, conditions
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet
kubectl get events -A --field-selector reason=PolicyRestored     # drift-correction activity
```

Six Prometheus metrics on `:8080/metrics` + Events on every condition transition: [`docs/reference/metrics-and-events.md`](docs/reference/metrics-and-events.md).

---

## Documentation

Full docs live under [`docs/`](docs/README.md), organized as a tree:

- **New here?** Follow the numbered path: [concepts](docs/getting-started/concepts.md) → [install](docs/getting-started/install.md) → [your first VirtualNetwork](docs/getting-started/first-vnet.md) (hands-on, with a live isolation probe).
- [`docs/guides/`](docs/README.md#guides): [recipes](docs/guides/recipes.md), [auto-allow](docs/guides/auto-allow.md), [operations](docs/guides/operations.md), [security](docs/guides/security.md), [troubleshooting](docs/guides/troubleshooting.md).
- [`docs/reference/`](docs/README.md#reference): look-up tables — [CRDs](docs/reference/api.md), [flags & values](docs/reference/configuration.md), [labels & annotations](docs/reference/labels-and-annotations.md), [metrics & events](docs/reference/metrics-and-events.md), [glossary](docs/reference/glossary.md).
- [`docs/internals/`](docs/README.md#internals): for contributors — [architecture](docs/internals/architecture.md), [source map](docs/internals/code-structure.md), [development](docs/internals/development.md).
- [`docs/faq.md`](docs/faq.md): cross-cutting Q&A. [`docs/adr/`](docs/adr/README.md): every accepted decision.

## Examples

Self-contained manifests; `kubectl apply -f` works on a fresh cluster:

| File | Demonstrates |
|---|---|
| [`config/samples/01_same_namespace.yaml`](config/samples/01_same_namespace.yaml) | Default: only pods in the home namespace can join. |
| [`config/samples/02_two_namespaces.yaml`](config/samples/02_two_namespaces.yaml) | `allowedNamespaces.names: [webapp, monitoring]` (explicit list). |
| [`config/samples/03_label_selector.yaml`](config/samples/03_label_selector.yaml) | `allowedNamespaces.selector: {{ matchLabels: {{ tier: prod }} }}` (label-based). |
| [`config/samples/04_all_namespaces.yaml`](config/samples/04_all_namespaces.yaml) | `allowedNamespaces.all: true` (wildcard: any namespace). |
| [`config/samples/05_disabled_namespace.yaml`](config/samples/05_disabled_namespace.yaml) | Per-namespace opt-out via `kube-vnet/disabled=true`. |
| [`config/samples/06_virtualnetworkbinding.yaml`](config/samples/06_virtualnetworkbinding.yaml) | `VirtualNetworkBinding`: enrolling pods without editing their labels. |
| [`config/samples/08_virtualnetworkbaseline.yaml`](config/samples/08_virtualnetworkbaseline.yaml) | `VirtualNetworkBaseline`: namespace-tier default memberships. |
| [`config/samples/09_clustervirtualnetworkbaseline.yaml`](config/samples/09_clustervirtualnetworkbaseline.yaml) | `ClusterVirtualNetworkBaseline`: the cluster-tier default (what the chart seeds). |

## Development & testing

```bash
make manifests generate  # regenerate CRDs, RBAC, deepcopy
make test                # unit tests (sub-second)
make integration-test    # envtest suite (~10s; needs Go only)
make e2e                 # kind end-to-end (needs Docker; CNI: kube-router or calico)
```

The three test rungs and CI lanes: [ADR 0018](docs/adr/0018-test-strategy-envtest-and-kind-calico.md). Source map: [`docs/internals/code-structure.md`](docs/internals/code-structure.md).

## Architecture decisions

Every significant choice is an ADR in [`docs/adr/`](docs/adr/README.md). The longer-form rationale lives in [`docs/internals/design.md`](docs/internals/design.md) (historical); where they disagree, the ADRs win.

## Status

`v1alpha1`. Single-cluster only. Generates plain `networking.k8s.io/v1` `NetworkPolicy`. The remaining gap to v1-complete is tracked in [ADR 0014](docs/adr/0014-deferred-v1-items.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
