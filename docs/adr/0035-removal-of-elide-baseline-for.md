# 0035 — Removal of `--elide-baseline-for`: baseline elide had no observable effect

Status: Accepted

Date: 2026-05-07

Refines: [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (which introduced the elide flag).

## Context

[ADR 0030](0030-unified-vnet-membership-with-resolution.md) introduced `--elide-baseline-for=<csv>` (default `cluster`) as a knob to skip the deny-all baseline for receiver pods on listed vnets. The mechanism added a `NotIn` matchExpression to the baseline's `podSelector` so that pods carrying e.g. `kube-vnet.system/net.cluster=both` would not be selected by the baseline.

The intent was: "pods on the cluster vnet are reachable from anywhere; don't redundantly deny them at the baseline." Sounds reasonable.

It turns out to be a no-op. NetworkPolicy semantics: a pod's effective ingress is the **union** of `from:` rules from all policies that select it. If no policy selects the pod, default-allow applies. The baseline contributes deny-all (zero allow rules); a pod selected by both the baseline AND a membership policy gets `(deny-all baseline) ∪ (allow X from membership) = (allow X)`. A pod selected by only the membership policy gets `(allow X)`. **Same effective ingress.** The baseline's selecting-or-not-selecting a pod that's already covered by a membership policy doesn't change connectivity.

This was discovered while exploring whether we could *meaningfully* reduce the policy count for the `cluster=default-both` pattern. The walkthrough surfaced a deeper observation: the elide knob was theatre. It changed which policies were said to "select" the pod but not what traffic was actually allowed.

The user's framing: you can't make a pod open by removing a policy that selects it. Open is achieved by attaching the pod to the cluster vnet (which generates the membership policy granting the allows). The elide knob was doing nothing.

## Decision

Remove the entire `--elide-baseline-for` mechanism:

- Operator flag `--elide-baseline-for` deleted from `cmd/main.go`.
- Chart value `operator.elideBaselineFor` deleted from `charts/kube-vnet/values.yaml` and the deployment template.
- Kustomize default `--elide-baseline-for=cluster` deleted from `config/manager/manager.yaml`.
- Field `BaselineElideFor` deleted from `NamespaceReconciler`.
- Parameter `elideFor` deleted from `DesiredBaseline` (signature becomes `DesiredBaseline(ns string)`).
- Field `OperatorNamespace` on `NamespaceReconciler` was added solely for the `cluster` elide entry's canonicalization; deleted alongside.
- Field `OperatorNamespace` on `ResolutionReconciler` was used by `CanonicalSuffix` for the same reason; with cluster collapsing to bare per [ADR 0033 Amendment](0033-canonical-fq-system-labels.md), nothing on the resolution path consults it. Deleted.
- Parameter `operatorNS` on `CanonicalSuffix` deleted — nothing consumes it anymore.

Baseline becomes uniformly deny-all selecting every pod in every managed namespace via `PodSelector: {}`.

## Consequences

- **No connectivity change** for any (src, dst) pair. The membership policy for the cluster vnet (or any other receiver-bearing vnet) continues to add the actual allow rules; the baseline continues to deny-all everything else.
- **Simpler operator surface**: one fewer flag, one fewer chart value, one fewer kustomize default.
- **Uniform baseline shape** across all clusters: `PodSelector: {}`, `policyTypes: [Ingress]`, no allow rules.
- **No migration tooling required**: existing baselines with the old `NotIn` matchExpression are re-rendered with the empty selector by SSA on the first reconcile after upgrade; drift correction handles the transition within one reconcile cycle.
- **Visible diff** for users running `kubectl get netpol -A -o yaml`: baseline `podSelector` changes from `{matchExpressions: [{key: kube-vnet.system/net.cluster, operator: NotIn, ...}]}` to `{}`. Effective ingress is unchanged.

## Alternatives considered

- **Keep the flag as a deprecated no-op for backwards compat.** Rejected: pure noise. Operators would set the flag, observe no effect, and file bug reports. Better to remove it cleanly.
- **Repurpose the flag to actually skip membership-policy emission.** Considered at length and rejected — see the dropped-options section in `i-have-created-a-drifting-yao.md`. Strict equivalence (cluster ON ≡ cluster OFF) plus meaningful policy reduction are fundamentally at odds for elide-list vnets in real clusters with asymmetric pods (`cluster=ingress` for ingress controllers, `cluster=egress` for cron jobs).
- **Document the flag's no-op nature without removing it.** Rejected for the same reason as the first alternative.

## References

- [ADR 0030](0030-unified-vnet-membership-with-resolution.md) introduced the flag.
- [ADR 0033](0033-canonical-fq-system-labels.md) (and its Amendment) canonicalized system labels and collapsed the cluster vnet to a bare key.
