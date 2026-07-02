# 0028 — Runtime policy-enforcement verification (design space)

Status: Proposed (draft)

Date: 2026-05-04

## Context

The operator's contract is "reconcile VirtualNetwork CRs to the desired set of NetworkPolicy resources at the apiserver." That contract ends at `kubectl get netpol`. Whether the CNI on each node then actually drops the packets the NetworkPolicy says it should drop is a different layer's responsibility.

In practice, users hit the gap several ways:

- kube-router with `ipMasq: true` masquerades intra-cluster traffic; podSelector matching breaks. Policies look right; enforcement is silently wrong.
- k0s's install-cniconf init container has a `[ ! -f ]` guard that prevents on-disk CNI config from being updated after first bootstrap; ConfigMap changes don't propagate. See [`../troubleshooting/cni-pitfalls.md`](../guides/cni-pitfalls.md) pitfall 2.
- kube-router service-proxy bootstrap deadlock after node reboot.
- Calico's Felix daemon not running.
- Cilium's identity-allocation lag in the first seconds after pod start.

Common shape: kube-vnet did everything right, but the user reaches for `kubectl describe vnet` and `kubectl describe netpol` first, sees nothing wrong, and assumes the operator is broken. Documentation alone (see [`../troubleshooting/cni-pitfalls.md`](../guides/cni-pitfalls.md)) helps but doesn't give a positive in-cluster signal that "yes, isolation is actually being enforced for this vnet right now."

This ADR is intentionally a draft. It exists to record the design space and reasoning so future contributors don't reopen the question without context.

## Decision space

Three options for surfacing "is enforcement actually working?" from the operator side. Listed from least to most invasive.

### Option A — Startup pre-flight

At operator process boot:

- List one or more `Node`s; check `Node.Status.NodeInfo` and the running pods in `kube-system` for a known CNI.
- If no NetworkPolicy-capable CNI is detected, log a single warning and emit a one-shot Kubernetes event on the operator's leader-election lease (or a ConfigMap, since there's no obvious "operator object" to attach an event to).
- If a CNI is detected, log a single info line naming it.

**Cost.** One API call at boot; runs once per process. Log-only.

**Catches.** "I forgot to install a CNI" / "I uninstalled my CNI by accident." Doesn't catch CNI misconfig.

**Recommendation.** Likely to ship. Bounded cost, useful signal, no probe traffic.

### Option B — `kube-vnet verify` opt-in subcommand

A new subcommand on the operator binary (`kube-vnet verify --vnet <name> --namespace <home-ns>`) that:

1. Spawns two probe pods in a target namespace (or accepts an existing pair via flags). One labelled to join the vnet, one not.
2. Waits for them to be Ready.
3. From the non-member, attempts a TCP connection to the member on a configurable port.
4. Reports pass/fail in structured form (JSON + exit code).
5. Tears down the probe pods.

Same shape as `cilium connectivity test` or `cmctl check api`. Invoked on demand by the user, by a CI job, or by a CronJob — *not* by the running operator process.

**Cost.** Zero in steady state; per-invocation only. Needs RBAC for pod create/delete in the target namespace, granted to whoever invokes it (a Job's ServiceAccount, the user's kubeconfig, etc.). Not added to the operator's own ServiceAccount.

**Catches.** Most enforcement failures: CNI not enforcing, CNI misconfigured (ipMasq, missing Felix), policies not generated for some reason, label-form typos.

**Recommendation.** Probably ship after a short design discussion covering: target-namespace selection (user-supplied, ephemeral, or pre-existing?), output format, exit-code semantics on partial failure, probe-pod image choice.

### Option C — Continuous in-operator probing

Operator pod itself sends probe traffic on a timer to a probe target inside each managed namespace; sets a `Status.Conditions[Enforced]` based on results.

**Rejected.** Several real costs without proportionate benefit:

- **Cross-layer ownership confusion.** When the CNI is broken, the operator goes Degraded. Cluster admins look at the operator's status, but the cause is in the CNI or distro layer. Two things complain about one root cause; ownership of "fix this" is muddied.
- **Probe side-effects.** Probe traffic in user namespaces is a surprise. Probe pods (or hijacked pods) require extra RBAC, possibly NET_ADMIN, and may be subject to the very policies they're supposed to verify (chicken-and-egg).
- **Flapping.** Probe traffic races with reconcile cycles, with pod startup, and with CNI identity allocation. Conditions flap Ready ↔ Degraded for transient reasons. Worse than no signal — it trains operators to ignore the operator's status.
- **Cost.** Continuous probing burns CPU and bandwidth in steady state.
- **Existing tools.** Sonobuoy with the NetworkPolicy conformance suite, `cilium connectivity test`, `kube-router`'s exposed metrics, and the upstream `network-policy-api` test suite all do this better and don't have the layering problem.

The pattern most mature operators follow is "manage the API state, expose metrics for what we manage, leave dataplane verification to specialised tools." cert-manager does this (`cmctl check api`); Cilium does this (`cilium connectivity test`); we should too.

## Recommendation

- **Now (this PR is documentation-only).** Ship the [`troubleshooting/cni-pitfalls.md`](../guides/cni-pitfalls.md) page and link it from the existing troubleshooting index. Documentation is the highest-leverage step and unblocks users who hit the known cases.
- **Later, small.** Implement Option A — startup pre-flight — as a separate PR. One log line per process boot, one event on the leader-election lease if no CNI is detected. Bounded cost.
- **Later, real feature.** Implement Option B — `kube-vnet verify` subcommand — as a separate PR after a small design doc. Cobra subcommand on the operator binary; reuses the existing apiserver client; spawns and tears down probe pods.
- **Never.** Option C. If a contributor proposes continuous in-operator probing, point them at this ADR.

## Consequences

**What we accept by not building Option C:** the operator's `Status.Conditions` will say `Ready=True` even when the CNI isn't enforcing the policies it manages. Users have to either run an external tool (sonobuoy / `cilium connectivity test` / our future `kube-vnet verify`) or read the docs to confirm enforcement.

**What we keep by not building Option C:** clean controller boundaries; no probe traffic in user namespaces; no flapping conditions during transient cluster events; no extra RBAC bleed onto the operator's ServiceAccount.

**What Option A buys us when it ships:** one positive signal at boot ("CNI X detected" / "no CNI detected"). Not enforcement verification, but catches the trivial "no CNI installed" case for free.

**What Option B buys us when it ships:** an on-demand, structured, automatable enforcement check. Useful in CI, in node-cordon-and-test runbooks, and in incident response. Not in the hot path of the running operator.

## Alternatives considered

- **Per-CNI deep validation** (e.g. parse kube-router's ConfigMap on every reconcile, warn on `ipMasq: true`). Rejected: distro-specific, brittle, the right fix is upstream and in the docs. We'd be playing whack-a-mole on every CNI's config schema.
- **A `kube-vnet/enforced-at` annotation on the vnet** that probes write back to. Rejected: same problems as Option C, plus mixes runtime probe state into the spec object's metadata.

## See also

- [`../troubleshooting/cni-pitfalls.md`](../guides/cni-pitfalls.md) — the documentation deliverable that lands before any of the implementation options.
- [ADR 0018 — Test strategy](0018-test-strategy-envtest-and-kind-calico.md) — kind+Calico e2e is the current way we verify enforcement *in CI*. Option B would extend that pattern into a runtime tool.
- [ADR 0019 — Baseline durability via drift correction](0019-baseline-durability.md) — example of a previous "should we add a runtime verifier?" decision that landed on "no, drift correction at the API layer is enough."
