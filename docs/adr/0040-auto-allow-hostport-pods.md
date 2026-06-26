# ADR 0040 — Auto-allow for hostPort pods via per-port stamping

**Status**: Accepted (2026-06-26)

## Context

ADR 0038 closed the external-traffic gap for Services (LoadBalancer / NodePort / ClusterIP+externalIPs) but deliberately deferred `hostPort`. The same default-deny problem applies to hostPort pods — once any `Ingress` NetworkPolicy selects them, external traffic to the node IP at the declared port is blocked — but the design needs more work than Service-source emission did:

1. **There's no Service to key on.** A `hostPort` declaration lives in the Pod's `spec.containers[*].ports[*]`; nothing else identifies the exposure.
2. **NetworkPolicy can't reference a pod by name.** The `podSelector` is label-based; pods don't carry naturally-unique stable labels. A new pod replacing the old one (Deployment rollout) gets a new name and may or may not share labels with its predecessor.
3. **A pod can declare multiple `(hostPort, protocol)` pairs.** Each is a distinct exposure (different iptables entries on the node); same-port-different-protocol pairs need distinct treatment.

## Decision

**Per-`(NS, port, protocol)` model** with operator label-stamping:

1. **Stamp**: the `ResolutionReconciler` (which already stamps `kube-vnet.system/net.*` membership labels) gains a parallel pass that stamps `kube-vnet.system/host-port.<port>.<protocol>=true` on every pod declaring `hostPort: <port> protocol: <PROTOCOL>` on any container. Lowercase protocol (`tcp`, `udp`, `sctp`). One stamp per distinct `(port, protocol)` the pod declares.
2. **Policy**: a new `HostPortReconciler` emits one NetworkPolicy per `(NS, port, protocol)` triple that appears anywhere in the namespace. Name shape per ADR 0039: `kube-vnet.ext.host.<port>.<protocol>-<8hex>`. Labels: `kube-vnet.system/role=external-allow`, `kube-vnet.system/source=host-<port>-<protocol>`. PodSelector matches the corresponding stamp; ingress allows `ipBlock: 0.0.0.0/0` on the specified `(port, protocol)`.

Example policy:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kube-vnet.ext.host.8080.tcp-a1b2c3d4
  namespace: traefik
  labels:
    kube-vnet.system/managed-by: kube-vnet
    kube-vnet.system/role: external-allow
    kube-vnet.system/source-kind: host        # dispatcher uses this label, not value parsing
    kube-vnet.system/source: host-8080-tcp    # symmetric with `svc-<name>` for Service-source
spec:
  podSelector:
    matchLabels:
      kube-vnet.system/host-port.8080.tcp: "true"
  ingress:
    - from:
        - ipBlock: { cidr: 0.0.0.0/0 }
      ports:
        - port: 8080
          protocol: TCP
  policyTypes: [Ingress]
```

A pod declaring two hostPorts gets two stamps and is matched by two policies:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: traefik-88786
  namespace: traefik
  labels:
    app.kubernetes.io/name: traefik
    kube-vnet.system/host-port.80.tcp: "true"   # stamped by ResolutionReconciler
    kube-vnet.system/host-port.443.tcp: "true"  # stamped by ResolutionReconciler
spec:
  containers:
    - name: traefik
      ports:
        - { hostPort: 80,  containerPort: 80,  protocol: TCP }
        - { hostPort: 443, containerPort: 443, protocol: TCP }
```

### Why per-`(NS, port, protocol)` and not per-pod

- **Pod identity is ephemeral.** Deployment ReplicaSet pods get new name suffixes on every rollout. Per-pod policies would churn on every restart — emit, delete, emit-again — for no gain.
- **The user-visible model fits.** "port X (TCP/UDP) on this node is open" is what users think about; "this specific pod is reachable on port X" is not. The policy's name reads exactly the user's mental model.
- **Pod population is automatic.** When a new pod inherits the same `hostPort`, the resolution controller stamps it on first reconcile (within milliseconds of pod create), and the existing policy's podSelector matches it. No policy update needed.
- **Stable across protocol distinction.** Same port at TCP vs UDP gets two policies, two stamps. A TCP-only pod never inadvertently becomes UDP-reachable just because some other pod in the NS uses UDP on the same port.

### Why we don't put the pod name in the policy

The natural-feeling alternative — stamp `kube-vnet.system/host-pod=<podname>` and emit `kube-vnet.ext.pod.<podname>-<hash>` — was rejected because pod names cycle, policies would churn, and the user mental model isn't "this pod is reachable" but "this port is reachable." See ADR 0039 for the naming convention; there is no `ext.pod` kind.

## Consequences

- Ingress controllers and admin daemons using hostPort (rather than Service: LoadBalancer) become reachable from outside the cluster by default in kube-vnet-managed namespaces. No annotation needed.
- Stamps under `kube-vnet.system/*` are protected by the system-labels VAP (ADR 0037) — users can't hand-add a stamp to spoof reachability or hand-remove one to hide a pod from auto-allow.
- Cleanup hook (ADR 0036) sweeps host-port policies on `helm uninstall` because they carry `kube-vnet.system/managed-by=kube-vnet`. No special cleanup logic needed.
- Same opt-out as Service-source: `kube-vnet/external-allow=false` on the Namespace (or `kube-vnet/disabled=true`) removes all host-port policies in that NS on the next reconcile.
- New per-NS reconcile work: when a hostPort pod is created/deleted, the operator does one list-and-diff in the NS. NS-bounded, cheap.

## Out of scope

- **`hostNetwork: true` pods.** Skipped — NetworkPolicy enforcement on host-network pods is CNI-dependent (Cilium yes, kube-router no, Calico mode-dependent); emitting a policy doesn't reliably help. Documented; pods exempt themselves by living in a `kube-vnet/disabled=true` namespace if they need to be reachable.
- **Per-Service-Port opt-out at the Service level**, since hostPort doesn't go through a Service. Opt-out is NS-wide for hostPort. If users need finer control, they can use `kube-vnet/disabled=true` on the namespace and write their own NetworkPolicy.
- **BGP-announced pod IPs without a Service or hostPort.** CNI-specific (Cilium, MetalLB BGP); pod is reachable on its pod IP without going through node-IP routing. Out of scope.

## Alternatives considered

- **Per-pod policies named `kube-vnet.ext.pod.<podName>-<8hex>`.** Rejected: pod names cycle on every Deployment rollout; per-pod policies would churn; user mental model is port-keyed, not pod-keyed.
- **Port-only stamps without protocol** (`kube-vnet.system/host-port.<port>=true`). Rejected: same port at TCP vs UDP would conflate into one stamp and one policy, allowing wrong-protocol reachability. The user explicitly raised this in design review.
- **One policy per NS with multi-port ingress + OR-ed podSelectors across stamps.** Rejected: NetworkPolicy `podSelector` can't OR across distinct matchLabels cleanly; per-(port,proto) keeps each policy focused on one exposure and is straightforward to reason about.
- **EndpointSlice-based selector synthesis for no-Service hostPort exposures.** Not applicable — hostPort lives entirely outside the Service/Endpoints model.

## Related ADRs

- **ADR 0037** — `kube-vnet.system/` prefix for operator-owned keys. The new `host-port.<port>.<proto>` stamps live under this prefix and inherit system-labels VAP protection.
- **ADR 0038** — Auto-allow externally-exposed Services. Its `Out of scope: hostPort` deferral is what this ADR closes.
- **ADR 0039** — Uniform kind-prefixed policy naming. `kube-vnet.ext.host.<port>.<proto>-<8hex>` is the form this ADR uses from day one.
- **ADR 0036** — Helm pre-delete hook. Cleans up host-port policies automatically (label-based).
