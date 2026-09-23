# ADR 0045 — Network wait for opted-in pods

**Status**: Accepted (2026-09-23)

Builds on: [ADR 0034](0034-admission-webhook-for-pod-resolution.md) (the stamping webhook injects the wait). Not a revival of [ADR 0028](0028-runtime-policy-verification.md)'s rejected Option C; see below.

## Context

With the stamping webhook on, a pod is a member of its vnets before it exists. On kube-router a live cluster still saw only 3 of 10 first connections succeed: traffic worked 2–4 s after container start. The rest of the delay is the CNI's, and it is structural:

- **kube-router learns a pod's IP only from `status.podIP`.** The kubelet publishes it after it has started the pod's containers, so kube-router can't start programming a normal pod until its app already runs.
- **kube-router applies rules in one pass.** On every pod event each node rebuilds all ipsets and its whole filter table from one view of the cluster (0.77–1.85 s per rebuild there). No version has an incremental mode.
- **Nothing reports "applied".** NetworkPolicy has no status (KEP-2943 was withdrawn); kube-router exposes only sync metrics. CNIs guarantee at most the local node before a pod starts.

An unprogrammed pod is denied, never over-permitted, so this is about convenience, not security. Clients that retry are unaffected. One-shot clients, such as a migration Job's first connection, fail.

## Decision

A pod opts in with the annotation `kube-vnet/network-max-wait: "<Go duration>"`. Its app is held until every node has applied its rules, and never longer than that duration. The chart value `webhook.networkWait.enabled` (default off, requires `webhook.enabled`) ships the parts and lets pods opt in. There is no global default and no global maximum.

- **Beacons.** A chart-owned DaemonSet runs a TCP listener on every Linux node (`network-beacon`, a subcommand of the operator binary), behind a headless Service. A chart-owned NetworkPolicy opens the beacons to every pod (`from: namespaceSelector: {}`), which kube-router compiles into an ipset of all pod IPs. A node's beacon therefore rejects a new pod until that node has rebuilt its rules with the pod's IP.
- **Injection.** On pod CREATE the mutating webhook prepends an init container `kube-vnet-network-wait` (the operator image, `network-wait` subcommand, PodSecurity-restricted, tiny requests and limits). It is skipped for hostNetwork pods and on UPDATE (init containers are immutable), and never added twice. An invalid or non-positive value, or an annotation on a cluster with the feature off, gives an admission warning and no wait. The pod is never rejected.
- **The wait.** Each round (~150 ms) it resolves the Service and dials every beacon that has not yet accepted. It ends when at least one beacon resolved and all resolved beacons have accepted, or when the maximum runs out. Either way it exits 0 and the app starts. Addresses are re-resolved every round, so a node that goes down stops being waited on.

**Why a beacon accepting means the vnet rules are live.** The beacons are a sync witness, not a vnet probe: the pod never talks to them over its vnet. Node N's beacon accepts only after N has rebuilt its rules with the pod's IP. That same one-pass rebuild put the pod into every membership rule on N, because the webhook stamped its labels at CREATE. Probing every Ready node covers the servers' nodes, and the pod's own node.

**Why an init container.** It is the only hook that runs after the pod has an IP and before its app. Admission and scheduling gates run before there is an IP. CNI chain plugins, NRI hooks and `postStart` block before the IP is published, so kube-router would never program it and the wait would deadlock. Being first also makes the kubelet publish the IP early, so kube-router programs the pod while the wait runs.

**How a beacon answers.** It completes the TCP handshake and closes; the handshake is the whole signal. ICMP is not covered by NetworkPolicy and needs `CAP_NET_RAW`. UDP has no handshake, so a rejection and a lost packet look the same.

## Why not Option C

ADR 0028 rejected continuous in-operator probing. This wait differs on every point that made Option C bad:

- the pod owner opts in, per pod;
- it runs once, inside the pod;
- it sets no operator status or condition, so nothing can flap;
- the operator gains no RBAC and sends no traffic.

## Alternatives considered

- **Per-CNI observers** (read kube-router's ipsets or iptables on each node). These need a privileged node agent and depend on kube-router's internal ipset naming. Rejected.
- **A fixed delay.** Always as slow as the worst case, and still a guess. The beacons usually release in the real programming time; the maximum is the fixed delay's fallback.
- **Fail closed** (keep the pod waiting until every beacon accepts). Rejected: a stuck node would block pod starts. The wait is a convenience, and when the maximum runs out the pod starts as it would without the wait.
- **Opt-out or a cluster-wide default.** Rejected: most pods retry and don't need the wait, and an injected init container is a visible change to the pod spec. The pod owner knows best.

## Consequences

- An opted-in pod starts its app once every node has applied it, typically within the real programming time and never later than its maximum. The wait's log names each beacon's time, or the beacons that never accepted.
- **The argument is kube-router's.** On Calico or Cilium a beacon accepting is a strong hint, not a proof, and the maximum still bounds the wait.
- **The first member of a vnet in its namespace** can, rarely, be released before kube-vnet's new membership policy is applied. kube-vnet creates the policy at pod CREATE, long before the pod has an IP, so in practice it lands first.
- **Not covered:** relabelling a running pod, and Windows nodes.
- **Egress policies and meshes.** A user egress policy or a traffic-intercepting mesh that blocks the probe makes the wait run to its maximum. kube-vnet's own policies are ingress-only.
- **Image pull is not bounded by the maximum.** The init container uses the operator image. If a node doesn't have it cached and the registry needs credentials the pod's namespace lacks, the pod is stuck in `ImagePullBackOff`. The beacons normally pre-pull the image on every node, and the default public image needs no credentials.
