# 0030 — Unified vnet-membership model with resolution layer

Status: Accepted (resolution-lattice section partially superseded by [ADR 0031](0031-baseline-tier-resolution.md); the `--elide-baseline-for` flag introduced here was removed by [ADR 0035](0035-removal-of-elide-baseline-for.md) — it had no observable effect on connectivity)

Date: 2026-05-05

Implementation rolled out across commits `66c8688` (ADR draft), `3481b8c` (direction-enum prune), `b232505` (CVNB CRD scaffold), `2fa49b1` (system vnets), `fb3121d` (resolution algorithm), `451a61c` (resolution controller), `01ca033` (generator switchover), `5e51392` (deny-all baseline + --elide-baseline-for), `0a49234` (system-labels VAP), `b7216a1` (--default-memberships flag).

The deprecated `--ingress-isolation*` flags and the `IsolationMode` enum remain in the codebase as vestigial input that no longer drives behaviour (the baseline is unconditionally deny-all + elide-list); their full removal is a follow-up cleanup pass and does not block this ADR's acceptance.

Supersedes: [ADR 0023](0023-decoupled-disabled-and-ingress-isolation.md), [ADR 0024](0024-ingress-isolation-mode-and-overrides.md), [ADR 0029](0029-allow-all-baseline-and-system-ns-disabled.md). Partially supersedes [ADR 0006](0006-baseline-default-deny-and-single-opt-out.md) (baseline shapes).

## Context

Today's operator carries four parallel knobs for "what does this pod's ingress look like by default":

- per-namespace `kube-vnet/ingress-isolation` annotation (`none|namespace|pod`)
- operator default `--ingress-isolation=…` and per-mode override lists
- per-namespace `kube-vnet/disabled=true` + operator-level `--disabled-namespaces`
- per-pod `kube-vnet/net.<vnet>=<direction>` labels
- `VirtualNetworkBinding` CRs as a parallel mechanism for label-less pods

The knobs overlap awkwardly: `mode` and the override lists carry allow/deny-with-exceptions structure; `disabled` is binary. The two structures don't compose. Compositions like "everyone in this NS can talk to everyone, plus a specific cluster-wide pool" require hand-written NetworkPolicies on top of kube-vnet's output.

## Decision

A single conceptual mechanism: a pod's effective vnet memberships compose via an inheritance lattice across four sources, with operator-managed system vnets covering the "default reachability" cases that previously needed per-mode baselines. The current isolation modes, override lists, baseline shapes, and binding-as-special-case all reduce to this one mechanism.

### Inheritance lattice

```
Operator default (lowest priority)
   ↓
ClusterVirtualNetworkBinding (cluster-scoped, new)
   ↓
VirtualNetworkBinding (namespace-scoped, exists today)
   ↓
Pod label (highest priority)
```

Each layer can:
- **Add** a new `(vnet, direction)` to the pod's effective membership.
- **Override** an inherited direction.
- **Opt out** via `direction=none`.

Within a single scope, multiple bindings may match the same pod. When directions agree, no conflict. When they disagree, a deterministic tiebreaker applies — alphabetical by binding name within scope. Conflicts are surfaced on the binding's `Conflicts` status condition, on the pod's `kube-vnet.system/conflict.<vnet>` annotation, and via the `kube_vnet_resolution_conflicts_total` metric.

### System vnets

Two vnets are operator-managed:

- `namespace` — one per managed namespace. Membership in this vnet at `direction=both` for every pod produces same-NS connectivity (the previous `mode=namespace`).
- `cluster` — one cluster-wide. Membership at `direction=both` produces allow-from-everywhere ingress (the previous `mode=none`).

Both are real `VirtualNetwork` CRs marked with `kube-vnet/system=true`, recreated on delete, removed on operator uninstall, protected from user mutation by a ValidatingAdmissionPolicy.

The previous three isolation modes map to three operator-default-memberships choices:

| Previous mode | New `--default-memberships=` |
|---|---|
| `none` | `cluster=both` (with `cluster` in default `--elide-baseline-for`) → every pod allow-all-via-cluster |
| `namespace` | `namespace=both, cluster=egress` → every pod accepts same-NS ingress |
| `pod` | `(empty)` → every pod gets the deny-all baseline; opt-ins via vnet membership |

### Two-prefix label scheme

Resolution layer reads:
- User-authored `kube-vnet/net.<vnet>=<direction>` labels (preserved; never modified by the operator).
- Bindings (cluster + namespace).
- Operator defaults.

Resolution layer writes:
- `kube-vnet.system/net.<vnet>=<direction>` labels (operator-owned). One per effective membership.
- Annotations for non-selector data (`kube-vnet.system/conflict.<vnet>`, `kube-vnet.system/resolved-generation`).

Generator reads only `kube-vnet.system/net.<vnet>` labels. The user's input space (`kube-vnet/`) and the operator's output space (`kube-vnet.system/`) are kept separate so ownership is unambiguous.

### Resolution controller, not webhook

A new controller watches the four input sources and patches pod labels via `pods` PATCH. No mutating admission webhook. RBAC: operator's ServiceAccount gains `pods` `patch` permission across managed namespaces.

Stamping is eventually-consistent. The race window (from pod admission to label-stamp, typically ~100ms) is handled by:
- **Initial creation: fail-closed.** The generator only includes pods that have at least one `kube-vnet.system/...` label or carry a `kube-vnet.system/resolved-generation` annotation. New pods are excluded from peer lists and selector matches until resolution completes for them. They're briefly unreachable rather than briefly over-permissive.
- **Updates: eventual-consistency window.** Documented; mitigated by a `resolved-generation` marker the generator can wait for. For sub-second revocation, users delete pods rather than flip labels.

If the race window becomes a real problem in practice, an opt-in mutating admission webhook can be added later. Not shipped initially.

### Baseline: always present, always deny-all, with an elide-list

The previous three baseline shapes (`pod`/`namespace`/`none`, with the recent ADR 0029 allow-all variant) collapse to a single shape:

- Always present in every managed namespace.
- Always deny-all (`policyTypes: [Ingress]`, no rules).
- PodSelector excludes pods that are receivers on any vnet listed in `--elide-baseline-for=<csv>` (default: `cluster`).

Concrete podSelector with the default elide-list:

```yaml
podSelector:
  matchExpressions:
    - key: kube-vnet.system/net.cluster
      operator: NotIn
      values: [both, ingress]
```

`NotIn` matches pods where the label is absent or has any other value (including `egress` or `none`). Pods with `kube-vnet.system/net.cluster=both` or `=ingress` are exempted from the baseline.

The elide-list is a cosmetic optimization — when a pod is a receiver on the cluster vnet (allow-from-everywhere), the baseline (deny-all) ∪ membership policy (allow-all) = allow-all anyway, so the baseline is a no-op. Eliding it just reduces the policy-object count.

### `ClusterVirtualNetworkBinding` (new CRD)

Cluster-scoped. Matches K8s convention (`ClusterRole` vs `Role`). Selects pods via `podSelector` + `namespaceSelector`.

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: ClusterVirtualNetworkBinding
metadata:
  name: cluster-default
spec:
  virtualNetworkRef:
    name: cluster
    namespace: kube-vnet-system
  namespaceSelector: {}     # all managed namespaces
  podSelector: {}           # all pods
  direction: egress
```

`VirtualNetworkBinding` (existing, namespace-scoped) keeps its current shape; an empty `podSelector: {}` selects all pods in the binding's namespace.

### `kube-vnet.system/` labels protected by VAP

A new `ValidatingAdmissionPolicy` rejects user attempts to set, change, or delete labels with the `kube-vnet.system/` prefix. The operator's ServiceAccount is exempted via a username-prefix check.

```cel
request.userInfo.username.startsWith(variables.operatorPrefix) ||
(
  (oldObject == null && !object.metadata.labels.exists(k, k.startsWith(variables.systemPrefix))) ||
  (oldObject != null &&
   object.metadata.labels.filter(k, k.startsWith(variables.systemPrefix)) ==
   oldObject.metadata.labels.filter(k, k.startsWith(variables.systemPrefix)))
)
```

Failure mode `Fail`. Same posture as the existing direction-value VAP from [ADR 0027](0027-pod-scoped-join-label-events.md).

### Direction enum pruned

Legacy aliases `true`, `false`, and the empty-string value are removed. The valid direction values are:

- `both` — pod accepts ingress and is an egress-capable peer.
- `ingress` — pod accepts ingress only (not an egress-capable peer).
- `egress` — pod is an egress-capable peer only (no ingress allow).
- `none` — pod is not a member (used to override inherited membership).

The direction-value VAP allow-list updates to `[both, ingress, egress, none]`. The generator's In-set selectors drop `true`: receivers `In: [both, ingress]`, initiators `In: [both, egress]`.

### `--disabled-namespaces` survives

The operator-level `--disabled-namespaces` allowlist is preserved as a privilege-boundary mechanism. Disabled namespaces get no kube-vnet objects of any kind — no system vnets, no membership policies, no baseline. The per-namespace `kube-vnet/disabled=true` annotation also survives with the same meaning.

### No migration

We're alpha. No automatic translation of the deprecated `--ingress-isolation*` flags or `kube-vnet/ingress-isolation` annotations. Users update manifests/values to the new shape on upgrade. CHANGELOG documents the breaking changes.

## Alternatives considered

These were walked through during the design discussion. Recording them here so future contributors don't re-tread the same arguments.

### Per-namespace `namespace-default-direction` knob driving both membership and baseline shape

Y in the discussion. One knob (the resolved direction default) determines both system-vnet membership and the baseline's shape via interpretation: `direction=ingress` is read as "deny-all baseline," `direction=egress` is read as "no baseline," etc.

**Rejected.** Overloading the direction enum with baseline-shape implications is confusing; a value's meaning at the per-pod level (`ingress` = "I receive only") doesn't align with its meaning as a default (`ingress` = "deny everything by default"). Two separate concerns shouldn't share a knob.

### Separate `Baseline` CRD or per-NS `namespace-default-policy` knob

X in the discussion. Two knobs: one for system-vnet defaults, one for the baseline shape. Composable but verbose, and most useful combinations turn out to collapse to the canonical mappings due to NetworkPolicy's additive-only semantics.

**Rejected.** User pointed out that always-deny-all baseline + the inheritance lattice naturally produces all the canonical modes without a separate knob. Adding a CRD for this feels excessive when the inheritance mechanism already covers it.

### `none` sentinel vnet for opt-out

A pseudo-vnet that, when joined, opts the pod out of all default memberships.

**Rejected.** Dummy-network feel; a special-case object that doesn't behave like a vnet. Per-vnet `direction=none` is more consistent with the existing direction enum.

### Pod-name-based selectors instead of label-based

Generate NetworkPolicies that match pods via `podSelector: matchExpressions: {key: <key>, operator: In, values: [pod-a, pod-b, …]}`.

**Rejected.** NetworkPolicy `podSelector` only matches labels; pod names aren't an implicit label, so we'd still need to stamp a `kube-vnet/pod=<name>` label. Worse, the `values` list churns on every pod CRUD (rewriting the policy and triggering CNI watch fanout), grows unboundedly with namespace size, and pod names aren't stable across re-creation.

### In/out label decomposition (`kube-vnet/in.<vnet>=true`, `kube-vnet/out.<vnet>=true`)

Resolve direction values into separate boolean labels for ingress and egress. Generator selectors become trivial (`MatchLabels: {kube-vnet.system/in.<vnet>: "true"}`).

**Rejected.** Doubles the label count per vnet membership. User explicitly objected to label noise on pods. The In-set selector machinery (`In: [both, ingress]`) is already simple enough.

### Mutating admission webhook for label stamping

Webhook stamps labels on pod admission, closing the race window completely.

**Deferred.** Webhook footprint (cert lifecycle, failure-policy, restart safety) is non-trivial. The race window via controller-managed stamping is bounded and fail-closed for new pods. If the eventual-consistency model proves insufficient for users with hard-security needs, a webhook can be added later as opt-in. Design captured in [ADR 0034](0034-admission-webhook-for-pod-resolution.md) (Proposed).

### Pod-level "no defaults" shortcut label

A single label like `kube-vnet/no-defaults: true` that opts a pod out of all default memberships at once.

**Deferred.** Per-vnet `direction=none` covers the case at the cost of two labels (`kube-vnet/net.namespace=none, kube-vnet/net.cluster=none`). Verbosity is acceptable for v1; we add the shortcut later if real users complain.

## Consequences

**What we gain.**

- Single conceptual mechanism. The user-facing model is "vnet memberships with inheritance." All previous concepts reduce to this.
- Compositions previously inexpressible become natural: "deny in this NS except a global pool" = `namespace=ingress, cluster=egress` cluster-default + `cluster=both` on the receivers.
- Centralized inheritance logic in the resolution controller; the generator becomes purely "labels in, NetworkPolicy out."
- Visibility: a user runs `kubectl get pod -o yaml` and sees their effective memberships in the `kube-vnet.system/` labels, no hidden state.
- Bindings stop being a parallel mechanism — they're inputs to the same resolver.

**What we trade.**

- Operator gains `pods` `patch` permission across managed namespaces. Documented escalation.
- Two label namespaces to learn (user input vs operator output).
- Race window during transitions; fail-closed for new pods.
- NetworkPolicy names will change again as the generator simplifies.
- All existing `--ingress-isolation*` configuration breaks; users update on upgrade. (Alpha allows this.)

## See also

- [ADR 0021](0021-direction-modes-on-join-labels.md) — direction enum (this ADR amends it: `true`/`false`/empty values dropped).
- [ADR 0026](0026-virtualnetworkbinding-crd.md) — `VirtualNetworkBinding` CRD (this ADR amends it: cluster-scoped variant added).
- [ADR 0027](0027-pod-scoped-join-label-events.md) — direction-value VAP (allow-list updated; new sibling VAP for `kube-vnet.system/` labels).
- [ADR 0028](0028-runtime-policy-verification.md) — runtime verification design space (orthogonal; unaffected).
- The implementation plan lives at `~/.claude/plans/i-have-created-a-drifting-yao.md` (off-repo).
