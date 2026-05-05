# 0027 — Pod-scoped events for join-label diagnostics

Status: Accepted

Date: 2026-05-04

## Context

The two most common adoption mistakes with the join label fail silently from the pod owner's point of view:

1. A pod carries `kube-vnet/net.<X>` (bare form) but no `VirtualNetwork` named `<X>` exists in the pod's own namespace. The operator sees no vnet to attribute the pod to, so the label is just an unknown key on the pod. The pod owner sees nothing.
2. A pod carries `kube-vnet/net.<homeNS>.<X>` (prefixed form) but `<homeNS>/<X>` doesn't exist (yet, or ever — typo). Same outcome: the operator has nothing to reconcile, no surface to complain on.
3. A pod carries the prefixed form for a vnet that *does* exist, but the pod's namespace isn't in that vnet's `spec.allowedNamespaces`. Today this surfaces on the *vnet's* `Degraded` condition with reason `InvalidJoiners` — but the audience for vnet conditions is the vnet owner, not the pod owner. A platform team running a shared `payments` vnet doesn't want to debug every misconfigured tenant pod through their own vnet's status.

The vnet-status surface (`InvalidJoiners`, `UnknownDirection`, `ConflictingDirections`) is correct for *vnet-owner* concerns: "something is wrong with my vnet's membership." It's the wrong surface for *pod-owner* concerns: "why isn't my pod a member?" Pod owners read pod-scoped output: `kubectl describe pod`, `kubectl get events --field-selector involvedObject.kind=Pod`. Nothing currently emits there.

A previous draft attempted to fold case (1) into the vnet status surface by mis-attributing bare-form pods in foreign namespaces to the nearest-named vnet anywhere in the cluster. Rejected: bare-form labels in foreign namespaces don't unambiguously identify a target vnet (`kube-vnet/net.payments` in namespace `webapp` could mean "the `payments` vnet in any namespace" — there's no way to know which one). Mis-attribution would produce false `Degraded` reports on innocent vnets.

Separately, direction-value typos (`kube-vnet/net.X=bothh`) are caught at reconcile time as `UnknownDirection`, but only after the pod is admitted and scheduled — by which point the pod owner has likely moved on. A *syntactic* check at admission time gives the user immediate feedback at `kubectl apply`.

## Decision

Split join-label diagnostics across two surfaces by problem class:

**Syntactic — admission-time, via `ValidatingAdmissionPolicy` (Kubernetes 1.30+).** A VAP shipped in the chart and the kustomize manifests rejects Pod create/update when any `kube-vnet/net.*` label has a value not in `[both, ingress, egress, none, true, false, ""]`. Helm-conditional on K8s ≥ 1.30 (the GA target for VAP). On older clusters the chart skips the VAP and the operator's existing `Degraded`/`UnknownDirection` reason still catches the same condition at reconcile time — the pod is admitted but excluded from membership.

**Stateful — reconcile-time, via Pod-scoped Warning Events.** A new `JoinLabelDiagnosticReconciler` watches Pods carrying any `kube-vnet/net.*` label and emits Warning events on the Pod itself for three conditions:

| Reason | Fires when |
|---|---|
| `BareJoinLabelVnetNotFound` | Pod has `kube-vnet/net.<X>` (bare form) but no `VirtualNetwork` of name `<X>` exists in the pod's *own* namespace. |
| `PrefixedJoinLabelVnetNotFound` | Pod has `kube-vnet/net.<homeNS>.<X>` (prefixed form) but the vnet `<homeNS>/<X>` doesn't exist. |
| `JoinLabelNamespaceNotAllowed` | Pod has the prefixed form for a vnet that exists, but the vnet's `spec.allowedNamespaces` doesn't permit the pod's namespace. |

Events go on the Pod object in the pod's own namespace. Standard pod-scoped tooling (`kubectl describe pod`, `kubectl get events --field-selector involvedObject.kind=Pod`) surfaces them.

The new controller skips pods in disabled or excluded namespaces (the `kube-vnet/disabled=true` annotation and the `--disabled-namespaces` flag are explicit opt-outs — emitting noise there would defeat the opt-out).

Direction-value validation is **not** in this controller. K8s ≥ 1.30 catches the same problem syntactically at admission; older clusters still see the existing vnet-status `UnknownDirection` reason. Emitting a third surface for the same fault would just be noise.

The existing vnet-status reasons (`InvalidJoiners`, `UnknownDirection`, `ConflictingDirections`) are unchanged. They serve the vnet-owner audience; the new pod events serve the pod-owner audience. Both surfaces are kept intentionally — they're not redundant, they're addressed to different people.

While we were touching join-label semantics, the empty-string direction value (`kube-vnet/net.X: ""`) was reinterpreted from `both` (the legacy "presence-only meant member" rule) to `none`. The VAP accepts the empty string as a *syntactically* valid value (so it doesn't reject existing manifests outright at admission), but the parser now treats it as "not a member." This is a breaking change for any manifest that relied on the empty string meaning member; users who intended membership should set an explicit `=both` (or the legacy `=true`).

## Why pod events vs other surfaces

| Alternative | Why not |
|---|---|
| **Vnet-status condition for foreign-NS bare-form labels** | Bare form in a foreign namespace doesn't identify a single vnet (mis-attribution risk). |
| **Validating webhook for vnet existence** | Pods can be admitted before their target vnet exists (templated installs, GitOps ordering); admission can't reliably check stateful conditions. |
| **Operator log lines** | Not addressable per pod; pod owners don't read operator logs. |
| **A dedicated `PodMembership` CRD** | Heavyweight; adds an apply-and-reconcile cycle for what is fundamentally a transient diagnostic. |
| **Mutating the pod (annotation, condition)** | The operator does not write to user resources (ADR 0022). |

Pod events are addressable to the right audience, transient (1-hour TTL by default), aggregator-friendly (`kube-state-metrics` events collector), and free of new resource types.

## Why admission for direction-value but not vnet-existence

Direction-value validation is purely *syntactic* — the set of accepted strings is fixed and static, doesn't depend on cluster state, and is decidable from the Pod object alone. Perfect fit for VAP.

Vnet-existence and `allowedNamespaces` are *stateful* — they depend on a `VirtualNetwork` resource that may not exist at the moment a pod is admitted (GitOps applies pods and vnets in the same commit; the order of admission isn't deterministic). Rejecting the pod at admission for "vnet not found yet" would break legitimate flows. These conditions are correctly handled at reconcile time, when the operator has a stable view of both pods and vnets.

## Consequences

- **Pro**: Pod owners get an actionable surface for the most common adoption mistakes, on the resource they own, via tools they already use.
- **Pro**: No mis-attribution. Each event names the specific vnet (or vnet name fragment) the pod's label refers to, scoped to the pod's own namespace.
- **Pro**: Admission-time syntactic check on K8s ≥ 1.30 catches typos at `kubectl apply` instead of at reconcile.
- **Pro**: No coupling to admission for stateful checks; pod ordering vs vnet ordering keeps working.
- **Con**: One additional controller to run (the diagnostic reconciler), even when no pods are misconfigured. Watch is filtered to pods carrying `kube-vnet/net.*` labels, so the steady-state cost is minimal.
- **Con**: VAP requires K8s 1.30+. Older clusters lose the admission-time surface and rely on the existing reconcile-time vnet-status reason. Acceptable degradation; the floor is what already exists today.
- **Con**: Events on pods in `kube-vnet/disabled` or excluded namespaces are not emitted by design. A user who annotates a namespace `disabled=true` and then wonders why their labels do nothing won't get an event. Mitigated by the same condition surfacing in `kubectl describe ns` (the annotation itself), and documented in the troubleshooting guide.

## Cross-references

- ADR 0021 — Direction modes on join labels. The VAP value-set mirrors the parser's accepted enum; the empty-string semantic shift is recorded as an addendum on ADR 0021.
- ADR 0022 — Long-form join label in the home namespace. Foreign-namespace bare-form misuse is now surfaced via `BareJoinLabelVnetNotFound` (recorded as an addendum on ADR 0022).
- ADR 0023 — Decoupled `disabled` and `ingress-isolation`. The diagnostic controller skips disabled/excluded namespaces by design — explicit opt-out trumps diagnostic noise.

## Addendum 2026-05-05 — VAP allow-list pruned; new sibling VAP for `kube-vnet.system/` labels

[ADR 0030](0030-unified-vnet-membership-with-resolution.md) requires two changes here:

1. **Direction-value VAP allow-list pruned.** The `validValues` list in the join-label-direction VAP becomes `[both, ingress, egress, none]`. The legacy `true`, `false`, and empty-string values are dropped (see also the [ADR 0021 addendum](0021-direction-modes-on-join-labels.md#addendum-2026-05-05--legacy-truefalseempty-aliases-dropped)).

2. **New sibling VAP: `kube-vnet-system-labels-protected`.** Rejects user attempts to set, change, or delete labels with the `kube-vnet.system/` prefix on Pod CREATE/UPDATE. The operator's ServiceAccount (`system:serviceaccount:<operator-ns>:<operator-sa>`) is exempted via a `request.userInfo.username` check, so the resolution controller's PATCH calls go through. Failure mode `Fail` — same posture as the direction-value VAP.

Together: the `kube-vnet/` label namespace is for user-authored input (validated by the existing direction-value VAP); the `kube-vnet.system/` namespace is for operator-managed output (locked down by the new VAP). Two complementary admission gates.

Both VAPs render via Helm and ship in the same chart template. Both are covered by the chart-manifest dry-run integration test (`chart_manifests_integration_test.go`), so a CEL syntax bug fails CI before a release ever ships.
