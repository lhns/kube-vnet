# ADR 0045 — Network wait for opted-in pods

**Status**: Accepted (2026-09-23)

> **Amendment (2026-09-23) — order of beacon and vnet rule, measured.** A manual experiment (the `e2e-experiment` workflow, `test/e2e/beacon_ordering_test.go`) measures the order directly. Probe pods join a vnet on the worker without the wait. From the pod's start, a sidecar dials the control-plane node's beacon and a vnet member on that node every few ms and records the first success of each; delta = vnet − beacon. 20 pods per CNI and run, under extra NetworkPolicy load and churn:
>
> | run | kube-router | Calico | Cilium |
> |---|---|---|---|
> | 300 policies, 10/s churn, 20 ms probe | 0 to +23 ms; 7 pods one probe round late | 0 on every pod | 0 to +3 ms |
> | 1000 policies, 20/s churn, 5 ms probe | −240 to −67 ms | −140 to +5 ms | no data (the load overflowed Cilium's policy map) |
> | 1000 policies on 20 ports, 20/s churn, 5 ms probe | −3195 to 0 ms | −690 to 0 ms | 0 to +1 ms |
>
> On kube-router the beacon and the vnet rule land in the same sync, but not at the same instant, and either can come first. In the one run where the vnet rule came second, it trailed by less than one 20 ms probe round. That is less than the kubelet takes to start the app container after the wait exits. On Calico the pod's own node usually opened all targets at once. Where the remote node came later, the vnet rule came before the beacon or within 5 ms of it. On Cilium every target was already open when the pod started. No run saw a vnet rule trail the beacon by more than one probe round. On Calico and Cilium the beacon remains a strong hint, not a proof, because both CNIs update each policy separately.

> **Amendment (2026-09-23) — tested on Calico and Cilium.** CI now runs the network wait tests on Calico and Cilium (`e2e-network-wait`) as well as kube-router. Two runs gave the same results; all tests pass on all three and none is skipped.
>
> | | kube-router | Calico | Cilium |
> |---|---|---|---|
> | hostNetwork pod to the remote beacon | refused | dropped (timeout) | dropped (timeout) |
> | hostNetwork pod to its own node's beacon | accepted | accepted | accepted |
> | wait released after (5 pods, 2 runs) | 3–241 ms | 1–97 ms | 2–119 ms |
>
> So the beacon is a witness on all three: a node admits a pod only once it knows the pod's IP as a pod. On kube-router that is the one-pass rebuild, so the vnet rules are live. On Calico (the pod in the selector's IP set) and Cilium (the pod's IP in the ipcache) it is the piece a vnet rule also needs, but those CNIs update each policy separately, so it stays a strong hint, not a proof. Because Calico and Cilium drop rather than reject, a beacon that has not yet accepted costs a round the 500 ms dial timeout instead of a quick refusal. The kind clusters in CI did not reproduce the race: all unwaited one-shot clients connected on every CNI, so the tests show the wait releases promptly and does not break the first connection; the evidence that it fixes the race is still the live kube-router cluster in Context.

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
- **The argument is kube-router's.** There, the vnet rules land in the same sync as the beacon but not at the same instant; when they came second, they trailed by milliseconds (see the amendment above). On Calico or Cilium a beacon accepting is a strong hint, not a proof, and the maximum still bounds the wait.
- **The first member of a vnet in its namespace** can, rarely, be released before kube-vnet's new membership policy is applied. kube-vnet creates the policy at pod CREATE, long before the pod has an IP, so in practice it lands first.
- **Not covered:** relabelling a running pod, and Windows nodes.
- **Egress policies and meshes.** A user egress policy or a traffic-intercepting mesh that blocks the probe makes the wait run to its maximum. kube-vnet's own policies are ingress-only.
- **Image pull is not bounded by the maximum.** The init container uses the operator image. If a node doesn't have it cached and the registry needs credentials the pod's namespace lacks, the pod is stuck in `ImagePullBackOff`. The beacons normally pre-pull the image on every node, and the default public image needs no credentials.
