# 0029 — Allow-all baseline in mode=none, and system namespaces disabled by default

> **Amendment (2026-07-09)**: the default disabled set is narrowed from `[kube-system, kube-public, kube-node-lease]` to `[kube-system]` by [ADR 0042](0042-coredns-ingress-carveout-and-kube-system-enrollment.md). `kube-public` and `kube-node-lease` hold no pods, so disabling them bought nothing; only `kube-system` (cluster-critical pods) stays disabled by default, and enrolling it is made DNS-safe by the chart's CoreDNS carve-out.

Status: Superseded by [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (2026-05-05). The "baseline always emitted for visibility" rule is replaced by "deny-all baseline + `--elide-baseline-for`" — visibility now comes from the system vnet membership labels rather than from a baseline placeholder. The "system namespaces disabled by default" decision is preserved unchanged in the new model.

Date: 2026-05-04

Supersedes the system-namespace-default decision in [ADR 0023](0023-decoupled-disabled-and-ingress-isolation.md). The decoupling principle in ADR 0023 stands; only the *default placement* of `kube-system`/`kube-public`/`kube-node-lease` and the *baseline shape in mode=none* change.

## Context

Two related defects in the previous design:

1. **Mode=none over-restricted vnet members.** The intended model is "isolation mode is the namespace's outer boundary; vnet membership adds peers within it." That model holds for `pod` (boundary = nothing; members = peers only) and `namespace` (boundary = same-NS; members = same-NS + peers), because the baseline policy in those modes contributes additively to member pods. In `none`, no baseline existed, so the membership policy (which selects member pods for ingress) clamped them down to peers-only. The pod's effective ingress was strictly tighter than the namespace's declared mode said it should be — contradicting the model.

2. **System-namespace default created visible kube-vnet objects in `kube-system`.** ADR 0023 placed the three system namespaces in the `none` override list (rather than the `disabled` list) so the operator could still discover deliberately-enrolled joiner pods there. With this ADR's first change (mode=none materializes a baseline), that placement would create an allow-all baseline NetworkPolicy in `kube-system`, `kube-public`, and `kube-node-lease`. Functionally invisible to traffic, but visible to `kubectl get netpol -n kube-system`, surprising for cluster admins, and a foothold for any future operator bug that flips the rule shape.

## Decision

Two coupled changes.

### 1. Mode=none materializes an allow-all baseline

`DesiredBaseline(ns, IsolationNone)` now returns a `NetworkPolicy` with `policyTypes: [Ingress]` and one empty ingress rule:

```yaml
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - {}                  # empty rule = "match all sources, all ports"
```

Per the K8s NetworkPolicy spec: *"An empty `NetworkPolicyIngressRule` matches all traffic."* This is the correct idiom for "allow all" — `from: [{}]` (an empty `NetworkPolicyPeer`) is rejected by the apiserver because each peer must specify at least one selector or IPBlock.

The membership-policy generator is unchanged. Member pods in mode=none now get baseline (allow-all) ∪ membership (vnet peers) = allow-all, restoring the outer-boundary model. Traffic outcome for non-members is identical to "no policy at all" in the previous design. The object's role is declarative: it announces "kube-vnet manages this namespace; mode is none."

### 2. System namespaces disabled by default

Helm chart (`charts/kube-vnet/values.yaml`) and the operator's `--disabled-namespaces` CLI default now list:

- `kube-system`
- `kube-public`
- `kube-node-lease`

The `ingressIsolation.namespaceOverrides.none` chart default and the `--ingress-isolation-none` CLI default are now empty.

To enroll a system-namespace pod in a vnet, an admin removes the relevant namespace from `disabledNamespaces` (and optionally adds it to a specific override list to choose its baseline mode). That's an opt-in configuration, not a default.

## Consequences

**Positive.**

- The outer-boundary model is now uniformly true across all three modes.
- `kubectl get netpol` is more informative: a managed namespace always has a baseline, so the absence of one means either "kube-vnet doesn't see this namespace" or "namespace is `disabled`." The distinction between "managed but unrestricted (`none`)" and "not managed (`disabled`)" becomes visible in the resource graph rather than only in operator config.
- System namespaces stay free of kube-vnet objects out of the box, matching the principle of least surprise for cluster admins.

**Costs.**

- Existing clusters running mode=none gain one additional NetworkPolicy per managed namespace on upgrade. The policy is functionally invisible — it allows everything — but it shows up in lists. CHANGELOG calls this out.
- One more object per managed namespace in steady state. Trivial; baselines are tiny and stable.
- The "discover deliberate joiner pods in `kube-system`" use case from ADR 0023 now requires explicit opt-in. Users who relied on the previous default to enroll a kube-system pod in a vnet must remove the namespace from `disabledNamespaces`. This is captured in CHANGELOG and in `docs/install.md`.

## Alternatives considered

- **Skip membership policy in mode=none** ("Option B" from earlier discussion). Equivalent for traffic, but mode=none namespaces have no kube-vnet objects at all, blurring the distinction with `disabled`. Rejected.
- **Document only; leave behavior as-is** ("Option C"). Doesn't fix the inconsistency; users continue to be surprised by mode=none over-restricting members. Rejected.
- **Embed isolation-level allows directly in membership policies, drop the baseline for members** ("Option A"). Self-contained per-pod policies. Functionally equivalent for traffic; non-members still need the baseline. The asymmetry isn't worth the duplication. Deferred (not rejected) — could be revisited if users want one-policy-tells-the-whole-story.

## See also

- [ADR 0023](0023-decoupled-disabled-and-ingress-isolation.md) — origin of the decoupling principle that `disabled` and `mode` are independent. This ADR amends 0023's default placement, not its principle.
- [ADR 0024](0024-ingress-isolation-mode-and-overrides.md) — operator default + per-mode override lists. The override shape is unchanged; only the chart-level defaults swap.
- [ADR 0025](0025-ingress-isolation-rename-egress-unrestricted.md) — egress is intentionally unrestricted by kube-vnet. Unchanged.
