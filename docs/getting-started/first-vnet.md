# Your first VirtualNetwork

A start-to-finish walkthrough: install the operator, create a network, join two pods, and *prove* the isolation works with a live probe. Every command is copy-pasteable.

If you haven't read [the concepts page](concepts.md) yet, the one-paragraph version: a `VirtualNetwork` is a named group. Pods join it with a label. Same-network pods can talk; everything else is blocked by an automatic deny-all baseline. The operator translates all of this into plain `networking.k8s.io/v1` `NetworkPolicy` — your CNI does the actual enforcement.

## 1. Prerequisites

- Kubernetes **1.25+** (the CRDs use CEL validation).
- A CNI that **enforces** `NetworkPolicy` — Calico, Cilium, kube-router, Antrea, and the managed-cloud variants all work; see [the compatibility list](install.md#prerequisites). If your CNI ignores NetworkPolicy, kube-vnet installs fine but isolates nothing.
- Helm 3 (for the recommended install path).

## 2. Decide your isolation level — before you install

This is the one decision the chart forces you to make. It seeds the cluster-wide default posture (a `ClusterVirtualNetworkBaseline`), and there is deliberately no default:

| Level | What it means | Choose it when |
|---|---|---|
| `cluster` | No isolation. Every pod auto-joins the cluster-wide system vnet in both directions — traffic flows exactly as before install. | **Adopting on an existing cluster.** Nothing breaks on day one; you tighten per-namespace or per-workload later. |
| `namespace` | Pods within the same namespace reach each other; cross-namespace needs explicit membership. Egress out of the namespace still works (DNS, image pulls, external APIs). | The common steady-state posture. |
| `pod` | Strict: a pod accepts ingress **only** through explicit vnet membership (label, binding, or baseline). Egress still permitted. | New clusters designed around kube-vnet, or high-isolation environments. |

Under the hood each preset seeds directions on the two system vnets (`namespace` and `cluster`) using overridable `default-*` values — so any namespace or pod can opt into a stricter or looser posture than the cluster default. Details: [concepts § the deny-all baseline](concepts.md#the-deny-all-baseline) and [ADR 0031](../adr/0031-baseline-tier-resolution.md).

**For this walkthrough, pick `cluster`** if you're on a cluster with existing workloads (safe), or `pod` if it's a scratch cluster and you want to see maximum isolation immediately. The walkthrough works with either — the probe results in step 6 differ only for the *unlabeled* pod.

## 3. Install

```bash
helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet \
  --version 0.1.0 \
  --namespace kube-vnet-system --create-namespace \
  --set operator.clusterBaseline.ingressIsolationLevel=cluster   # or: namespace | pod
```

The values a new user actually needs to know about (everything else has sensible defaults — full list in [the configuration reference](../reference/configuration.md)):

| Value | Default | When to set it |
|---|---|---|
| `operator.clusterBaseline.ingressIsolationLevel` | *(none — required)* | Always. The isolation decision from step 2. |
| `operator.disabledNamespaces` | `[kube-system, kube-public, kube-node-lease]` | Add namespaces the operator should never touch. The release namespace is always excluded implicitly. |
| `operator.apiserverSourceCIDR` | `0.0.0.0/0` | Tighten to your control-plane subnet if your pod network is externally reachable. See [auto-allow](../guides/auto-allow.md#apiserver-reachable-services). |
| `replicaCount` | `1` | Set `2` for HA (leader election is already on). |

Other install paths (plain `kubectl apply`, kustomize, air-gapped, signature verification): [install.md](install.md).

## 4. What's now in your cluster

Four CRDs (group `kube-vnet.lhns.de/v1alpha1`):

| Kind | Scope | Short names | Constraint | Purpose |
|---|---|---|---|---|
| `VirtualNetwork` | Namespaced | `vnet`, `vnets` | DNS-1123 name | A named network. Lives in the namespace that owns it; `spec.allowedNamespaces` controls who may join from elsewhere. |
| `VirtualNetworkBinding` | Namespaced | `vnb`, `vnbs` | non-empty `podSelector` | Attach pods to a vnet **without editing their labels** — for third-party charts. Selects pods in its own namespace. |
| `VirtualNetworkBaseline` | Namespaced | `vnbl`, `vnbls` | singleton, named `default` | Namespace-wide default memberships every pod in the NS inherits. |
| `ClusterVirtualNetworkBaseline` | Cluster | `cvnbl`, `cvnbls` | singleton, named `default` | Cluster-wide default memberships — this is what the chart seeded in step 3. |

Two system vnets exist without you creating anything:

- **`namespace`** — one per managed namespace; joining it means "reachable by same-namespace pods".
- **`cluster`** — one for the whole cluster (lives in the release namespace); joining it means "reachable cluster-wide".

And every `NetworkPolicy` the operator emits follows one naming grammar, so you can always tell what a policy is at a glance:

```
kube-vnet.base                             deny-all ingress baseline (one per managed NS)
kube-vnet.mem.<homeNS>.<vnet>-<8hex>       membership policy for a vnet
kube-vnet.ext.svc.<service>-<8hex>         auto-allow: externally exposed Service
kube-vnet.ext.host.<port>.<proto>-<8hex>   auto-allow: hostPort pod
kube-vnet.ext.apiserver.<service>-<8hex>   auto-allow: Service the apiserver dials
```

Sanity check:

```bash
kubectl get pods -n kube-vnet-system                # operator Running
kubectl get crds | grep kube-vnet                   # 4 CRDs
kubectl get cvnbl default -o yaml                   # the seeded cluster baseline
```

## 5. Create a network and join two pods

```bash
kubectl create namespace demo

cat <<'EOF' | kubectl apply -f -
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: payments
  namespace: demo
---
apiVersion: v1
kind: Pod
metadata:
  name: server
  namespace: demo
  labels:
    kube-vnet/net.payments: "both"        # ← joins the payments network
spec:
  containers:
    - name: web
      image: nginx:alpine
      ports: [{ containerPort: 80 }]
---
apiVersion: v1
kind: Pod
metadata:
  name: client
  namespace: demo
  labels:
    kube-vnet/net.payments: "both"        # ← also a member
spec:
  containers:
    - name: shell
      image: alpine:3
      command: ["sleep", "infinity"]
---
apiVersion: v1
kind: Pod
metadata:
  name: outsider
  namespace: demo                          # ← NO join label
spec:
  containers:
    - name: shell
      image: alpine:3
      command: ["sleep", "infinity"]
EOF
```

Watch the operator react:

```bash
kubectl get vnet payments -n demo                   # Ready=True
kubectl get networkpolicy -n demo
# NAME                              ...
# kube-vnet.base                    ← the deny-all baseline
# kube-vnet.mem.demo.payments-...   ← the membership policy
```

The operator also *stamped* the member pods — look at `server`'s labels and you'll see `kube-vnet.system/net.demo.payments: both` next to your own label. Your `kube-vnet/…` label is the **request**; the operator-owned `kube-vnet.system/…` stamp is the **confirmed membership** the policies select on. You can't set system-prefix labels yourself (an admission policy rejects them).

## 6. Prove it with a probe

```bash
SERVER_IP=$(kubectl get pod server -n demo -o jsonpath='{.status.podIP}')

# Member → member: must succeed
kubectl exec -n demo client -- wget -qO- --timeout=3 "http://$SERVER_IP" >/dev/null \
  && echo "client → server: OK (same network)"

# Non-member → member: depends on your isolation level
kubectl exec -n demo outsider -- wget -qO- --timeout=3 "http://$SERVER_IP" >/dev/null \
  && echo "outsider → server: reachable" \
  || echo "outsider → server: BLOCKED"
```

What you should see:

| Isolation level from step 2 | `client → server` | `outsider → server` |
|---|---|---|
| `pod` | OK | **BLOCKED** — the outsider has no membership at all |
| `namespace` | OK | reachable — same-namespace traffic is allowed by the seeded default |
| `cluster` | OK | reachable — nothing is isolated yet by design |

If you installed with `namespace` or `cluster` and want to see the strict behavior without reinstalling, opt the `demo` namespace into the strict posture — this is the baseline-tier model doing its job:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetworkBaseline
metadata:
  name: default            # singleton per namespace
  namespace: demo
spec:
  memberships:
    - virtualNetworkRef: { name: namespace, namespace: kube-vnet-system }
      direction: none      # opt out of same-namespace reachability
    - virtualNetworkRef: { name: cluster, namespace: kube-vnet-system }
      direction: egress    # keep outbound (DNS etc.), accept nothing
EOF
# rerun the outsider probe → BLOCKED
```

(Adjust `namespace: kube-vnet-system` if you installed into a different release namespace.)

**Heads-up on already-open connections**: NetworkPolicy is enforced when a connection is *established*. If a connection existed before you tightened anything, Linux conntrack keeps it alive. Restart the client pod if a probe surprisingly succeeds — details in [the FAQ](../faq.md#i-tightened-isolation-but-existing-cross-namespace-connections-still-work-why).

## 7. The three ways to join a network

You've used the first; the other two cover cases where you can't (or shouldn't) edit pod labels:

1. **Pod label** — `kube-vnet/net.<vnet>: <direction>` for a vnet in the pod's own namespace, or `kube-vnet/net.<homeNS>.<vnet>: <direction>` for a vnet elsewhere. Direct, visible in the pod spec. ([sample 01](../../config/samples/01_same_namespace.yaml), [sample 02](../../config/samples/02_two_namespaces.yaml))
2. **`VirtualNetworkBinding`** — a CR that selects pods by label and attaches them; the pods themselves stay untouched. The tool for third-party Helm charts. ([sample 06](../../config/samples/06_virtualnetworkbinding.yaml))
3. **Baselines** — namespace-wide (`VirtualNetworkBaseline`) or cluster-wide (`ClusterVirtualNetworkBaseline`) default memberships that every pod inherits, unless a lower tier overrides them. ([sample 08](../../config/samples/08_virtualnetworkbaseline.yaml), [sample 09](../../config/samples/09_clustervirtualnetworkbaseline.yaml))

When several sources disagree about the same pod and vnet, resolution is **fail-closed**: within a tier, directions intersect; across tiers, a lower tier may override only values the upper tier marked `default-*`. Full rules: [concepts § direction modes](concepts.md#direction-modes-on-the-join-label).

## 8. Directions

The label/binding/baseline *value* declares which way traffic flows for that pod on that network:

| Value | Meaning | Legal on |
|---|---|---|
| `both` | accept ingress from members AND initiate to members | pod label, binding, baseline |
| `ingress` | accept only (e.g. a read-only API) | pod label, binding, baseline |
| `egress` | initiate only (e.g. a metrics shipper) | pod label, binding, baseline |
| `none` | not a member; also cancels inherited memberships | pod label, binding, baseline |
| `default-both` / `default-ingress` / `default-egress` / `default-none` | same as the bare value, but **overridable** by lower tiers | baselines only |

## 9. Cross-namespace networks

By default only pods in the vnet's own namespace may join. `spec.allowedNamespaces` widens that — it is **join eligibility, not blanket access**; pods in allowed namespaces still need a membership:

```yaml
spec:
  allowedNamespaces:
    names: [webapp, monitoring]          # explicit list        (sample 02)
    # selector: { matchLabels: { tier: prod } }   # by NS label (sample 03)
    # all: true                                   # any namespace (sample 04)
```

Multiple matchers union; the home namespace is always included. See [concepts § cross-namespace reach](concepts.md#cross-namespace-reach-allowednamespaces).

## 10. What the operator handles without you asking

The deny-all baseline would break three kinds of legitimate traffic that never match a pod-selector rule — so the operator detects and allows them automatically ([full guide](../guides/auto-allow.md)):

- **Externally-exposed Services** (LoadBalancer / NodePort / externalIPs) — your ingress controller keeps working.
- **hostPort pods** — node-port-bound daemons stay reachable.
- **Services the apiserver dials** — admission webhooks (cert-manager, kyverno, …), metrics-server and friends keep answering.

Each emits a visible, labeled `kube-vnet.ext.*` policy you can inspect, and each has an opt-out annotation.

Also automatic: **drift correction** (hand-edits to operator-managed policies are reverted), **cleanup** (deleting a vnet removes every policy it generated), and **hands-off namespaces** (`kube-vnet/disabled=true` annotation or `operator.disabledNamespaces`).

## 11. Clean up the demo

```bash
kubectl delete namespace demo
```

## Where next

- Real-world patterns (three-tier app, shared observability network, migration of an existing namespace): [recipes](../guides/recipes.md)
- Every field, flag, label, and metric: [reference/](../reference/)
- Running it in production (HA, alerts, playbooks): [operations](../guides/operations.md)
- Something didn't work: [troubleshooting](../guides/troubleshooting.md)
