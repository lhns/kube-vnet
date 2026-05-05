# 0032 — Removing a chart-shipped CRD requires a two-release deprecation dance

Status: Accepted

Date: 2026-05-05

## Context

Every CRD shipped under `charts/kube-vnet/templates/crd-*.yaml` carries the `helm.sh/resource-policy: keep` annotation. This is non-negotiable for a stateful operator: without `keep`, a `helm uninstall` cascade-deletes the CRD, which in turn cascade-deletes every CR (every `VirtualNetwork`, every binding, every baseline). A careless `helm uninstall && helm install` to test something would silently destroy every workload's vnet membership across the cluster. ADR 0030 / the chart-RBAC fix introduced templated CRDs with `keep` precisely for this reason.

But `keep` has an inverse cost we hit when removing `ClusterVirtualNetworkBinding` in the ADR-0031 cleanup PR: Helm's reconciler treats `keep` as "leave this object alone on upgrade, regardless of whether the chart still ships it." When release N+1 removes the CRD's template entirely, the live CRD on the user's cluster is orphaned — Helm no longer manages it, and the `keep` annotation is permanent on the live object. The user is left to `kubectl delete crd …` manually, which is easy to miss; the orphan CRD continues to accept `kubectl apply` of CRs whose schema we no longer support.

The CVNB removal landed in a single commit. Existing users (the maintainer's own cluster, plus anyone running a `0.0.0-dev.<sha>` build that included CVNB) will carry an orphan `clustervirtualnetworkbindings.kube-vnet.lhns.de` after the next `helm upgrade`. There's no harm done if they read the CHANGELOG, but contributors need a written rule so the next CRD removal isn't a repeat.

## Decision

Removing any CRD shipped under `charts/kube-vnet/templates/crd-*.yaml` requires two releases:

- **Release N (deprecation)**: edit the CRD's chart template to drop the `helm.sh/resource-policy: keep` annotation. The template still ships the CRD; the only change is that the live object is no longer protected. Add a CHANGELOG entry under "Deprecated" naming the CRD and stating: "removed in next release; the keep annotation is dropped now so the next upgrade cleans up automatically — no `kubectl delete` needed."
- **Release N+1 (removal)**: remove the chart template entirely. CHANGELOG under "Removed" confirms the auto-cleanup happens on this upgrade.

On the upgrade to N+1, Helm sees: "previous release contained this CRD, current release doesn't, the live object has no `keep` annotation" — and deletes it. The user does nothing.

This rule applies only to CRDs (where the orphan-state can confuse the apiserver — clients can still apply now-unsupported CRs against the dead schema). For other `keep`-annotated resources (currently only the seeded `ClusterVirtualNetworkBaseline`), orphaning is benign and intentional: `keep` is exactly the annotation for "user might still want this object even after uninstalling the chart."

## Consequences

- **Clean automatic CRD removal**, in exchange for two releases of overhead. CHANGELOG audit becomes the gate; a contributor reviewing a "Removed: CRD X" entry should confirm the previous release shipped a "Deprecated: CRD X" entry that dropped the keep annotation.
- **No chart-side migration code needed.** No pre-delete hooks, no migration Jobs, no operator-side cleanup logic. The cost is purely process discipline.
- **Mistakes are recoverable but ugly.** If a contributor removes a CRD in one step (as we did with CVNB), the only fix is: (a) document the manual cleanup in CHANGELOG so users know to `kubectl delete crd …`; or (b) re-add the template without `keep` for one release, then remove again — bigger churn than just doing the dance correctly the first time.
- **The rule is enforced by review, not CI.** A future CI lint that walks the previous tag's chart and fails if any CRD lost a template without first losing its keep annotation is plausible (a single `git show <prev-tag>:charts/kube-vnet/templates/crd-*.yaml` diff against the current tree) but adds infrastructure for what's currently a once-a-year operation. Revisit if CRD removals become routine.

## Alternatives considered

- **Pre-delete Helm hook that runs `kubectl delete crd <name>`.** Rejected: the hook needs to live somewhere — either in the chart forever (cruft per removed CRD) or removed in another release dance (more overhead than dropping the annotation directly). Hooks also run in a fresh pod with their own RBAC, adding deployment surface for a one-shot operation.
- **Drop `keep` from CRDs entirely.** Rejected: cascade-delete on `helm uninstall` is the catastrophically-worse failure mode. Any user who's done `helm uninstall && helm install` (a common debugging move) would lose every vnet on the cluster.
- **Trust contributors to remember the rule without writing it down.** Rejected: this PR is exhibit A for why that doesn't work — the maintainer who introduced the templated-CRD-with-keep pattern (ADR 0030 / chart-RBAC fix) is the same person who removed CVNB in one step three months later.
- **CI lint enforcing the two-release rule by walking git history.** Worth doing eventually; out of scope here. A written rule plus CHANGELOG-entry review covers the case for now.

## Out of scope

- Same problem theoretically applies to *any* chart-shipped resource with `keep` (currently the seeded `ClusterVirtualNetworkBaseline`, plus the CRDs). Plain CRs orphan benignly — no schema-mismatch risk, no apiserver confusion. `keep` on a CR is intended specifically for "user might still want this object after uninstalling the chart."
- Retroactively fixing the CVNB removal to follow the pattern. Would need a release that re-adds the CVNB template without `keep`, then another to remove it — unjustified churn; documenting the manual `kubectl delete crd` step in the CHANGELOG is acceptable for the small population of pre-1.0 users.
