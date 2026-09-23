# 0034 — Mutating admission webhook for synchronous pod-tier resolution

> **Amendment (2026-09-11, revised 2026-09-23) — implemented as an opt-in (`webhook.enabled`); the "VAP exemption" section below is wrong.**
>
> **The window was measured.** A `vmctl` migration Job could not reach its target on its first connection and succeeded on retry. On kube-router v2.10.0 a kube-vnet denial is an immediate RST, not a timeout: a control pod without the join label was refused 60/60 over ~2 min, and the pod IP was refused too, which rules out endpoint readiness. Probe granularity bounds kube-vnet's share of the window at <=1s. It is not the whole window: a later test on the same cluster, with the webhook on, found pods stamped at creation yet only 3 of 10 first connections succeeding. Traffic works 2.15-3.64s after container start, because kube-router rewrites its full iptables ruleset on every pod event (0.77-1.85s per rewrite, growing with the number of NetworkPolicies). The webhook removes kube-vnet's delay, not the CNI's; a client that connects at startup still needs to retry. A separate field report, a pod created before its vnet that was never stamped, was *not* this window. It was the missing `VirtualNetwork` watch fixed in [ADR 0030](0030-unified-vnet-membership-with-resolution.md)'s 2026-07-26 amendment, which no webhook could have fixed.
>
> **The VAP blocker.** A mutating webhook's patch is attributed to the original requester, and mutating webhooks run before validating admission policies. With the `system-labels-vap` pods rule in place (`failurePolicy: Fail`; on CREATE `oldObject` is null, so any system label is denied), a stamping webhook would fail every pod creation in a managed namespace. So when the webhook is enabled, the pods it admits are policed by a *validating webhook* backed by the same resolver instead. *(Correction, 2026-09-23: the VAP keeps its pods rule, narrowed by a `matchCondition` to pods the webhooks' selectors skip — the excluded namespaces and pods labelled with the chart's `app.kubernetes.io/name` — so no pod is left unchecked. Initially the rule was dropped outright, which left those pods unprotected.)* It enforces a stronger property than CEL can: a pod's `kube-vnet.system/*` labels must equal what `Resolve()` produces. Only the delta the request introduces is policed, so a stale stamp the request didn't touch can't cause a collateral denial. The VAP's `networkpolicies` and `virtualnetworks` rules are untouched, and with `webhook.enabled=false` the chart renders as before.
>
> **The mutator writes and prunes only stamps the request left untouched.** A stamp the request itself supplied stays as submitted, so the validator rejects a wrong value with guidance, as the VAP does without the webhook, instead of the mutator silently rewriting it (which it did until 2026-09-23, found in the field test above). The reconciler has no requester to answer to and owns every stamp. Both write through the same `controller.SyncStamps`, differing only in which stamps they own.
>
> **failurePolicy is split.** Mutating is `Ignore`: it is an acceleration, and an outage degrades to pre-webhook behaviour. Validating is `Fail`, keeping the guarantee that stamps cannot be forged while the operator is down. That is an availability regression, since an operator outage blocks pod creation in managed namespaces. `kube-system`, `kube-public`, `kube-node-lease` and the release namespace are excluded so the cluster and the operator can recover, and this is why the feature is opt-in.
>
> **No bootstrap loop.** `cmd/main.go` always appends the operator's namespace (`POD_NAMESPACE`, which `--webhook-enabled` requires) to the disabled list. That namespace gets no baseline, so the webhook Service needs no [ADR 0041](0041-auto-allow-apiserver-reachable-services.md) auto-allow.
>
> **Corrections to the text below.** The Context describes the baseline selecting unstamped pods through a `NotIn` matchExpression. [ADR 0035](0035-removal-of-elide-baseline-for.md) removed that: the baseline now selects every pod (empty `podSelector`) and membership policies grant. The fail-closed property is unchanged. The "Handler shape" sketch shipped as `mutator.go` and `validator.go` in `internal/webhook/podresolution/`, both calling the shared `controller.Resolver` that the reconciler also uses. Restamps caused by config edits (Binding, Baseline or VirtualNetwork changes) stay asynchronous, because no pod admission request sees them.
>
> **Scheduling gates were evaluated and rejected (2026-09-23).** Injecting `spec.schedulingGates` at admission and letting the reconciler stamp and then ungate would keep the admission policy as the only label-protection model and remove the fail-closed dependency. It would also delay every pod start by a reconcile and could not stamp a relabelled running pod synchronously. Instant stamping on create and relabel was judged worth the second protection model. Both modes run the full e2e suite, so the split stays tested.

Status: Accepted

Date: 2026-05-06

Refines: [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (which deferred the webhook as future work). Reuses the resolution model from [ADR 0031](0031-baseline-tier-resolution.md) and the canonical FQ label shape from [ADR 0033](0033-canonical-fq-system-labels.md).

## Context

Today, when a pod is admitted to a managed namespace, there is a brief window (typically ~100ms — eventually-consistent per [ADR 0030 § "Resolution controller, not webhook"](0030-unified-vnet-membership-with-resolution.md#resolution-controller-not-webhook)) between apiserver persistence and the operator's `ResolutionReconciler` stamping the canonical FQ system labels (`kube-vnet.system/net.<homeNS>.<vnet>=<dir>`) and the `kube-vnet.system/resolved-generation` annotation.

Two safeguards already exist:

- **Baseline fail-closed safety net** (`DesiredBaseline` in `internal/controller/baseline.go`). The deny-all baseline `NetworkPolicy` selects every pod whose `kube-vnet.system/net.*` labels are *absent* (the `NotIn [both, ingress]` matchExpression also matches a missing label). Unresolved pods are therefore selected by the baseline → deny-all → no inbound traffic. **Security posture: unresolved pods are unreachable, never over-permissive.**
- **Generation gate on membership inclusion** (`discoverMembers` in `internal/controller/virtualnetwork_controller.go` skips pods missing `kube-vnet.system/resolved-generation`). Prevents an unresolved pod from being added as a peer to other vnets' membership policies until the operator has signed off on its resolution.

These safeguards close the security hole but leave a liveness hole:

- **Receiver-side liveness gap.** During the resolution window, a legitimately-membership-bearing pod is *unreachable by its peers*: the baseline denies inbound, and the membership policy that should allow inbound from peers either doesn't exist yet or doesn't yet name the new pod's NS in its peer list. Failed connections, retried requests, p99 latency spikes for any service that scales up under load.
- **Sender-side visibility gap.** A new pod's outbound peers don't add it to their `from:` rules until the operator reconciles every affected vnet's membership policy. Even after the new pod's labels are stamped, the *peer* policies haven't yet been refreshed — that's a second reconcile window, additive on top of the first.
- **Pod-edit window.** `kubectl label pod` (or any controller-driven label edit) triggers re-resolution by the same controller path, with the same race characteristics.

[ADR 0030 § "Mutating admission webhook for label stamping"](0030-unified-vnet-membership-with-resolution.md#mutating-admission-webhook-for-label-stamping) explicitly deferred the webhook as future opt-in work, citing cert-lifecycle and restart-safety overhead. It noted: *"If real-world users need sub-second guarantees, a future opt-in webhook can be added."* This ADR captures the design for that opt-in. Status remains **Proposed** until the design is validated against a real workload.

## Decision

A `MutatingAdmissionWebhook` on `pods` CREATE and UPDATE that runs the same pure `Resolve()` from `internal/controller/resolution.go` against the controller-runtime cache (already populated for the `ResolutionReconciler`) and patches the canonical FQ system labels + the resolved-generation annotation onto the incoming pod object before admission completes.

### Webhook configuration

```
scope:                      Namespaced
operations:                 [CREATE, UPDATE]
resources:                  [pods]
failurePolicy:              Ignore
reinvocationPolicy:         IfNeeded
sideEffects:                None
admissionReviewVersions:    [v1]
```

`failurePolicy: Ignore` is load-bearing — see "Failure semantics" below.

### Handler shape

New package `internal/webhook/podresolution/handler.go`:

```go
type Handler struct {
    Client            client.Reader        // controller-runtime cache reader
    NSFilter          *controller.NamespaceFilter
    OperatorNamespace string
    LabelPrefix       string               // default "kube-vnet/"
    decoder           admission.Decoder
}

func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
    // 1. Decode the pod from req.Object.
    // 2. Skip if pod's namespace is not managed (NSFilter check on cached Namespace).
    //    Skip if pod carries label app.kubernetes.io/name=kube-vnet (operator self-exclude).
    // 3. Build BaselineRules (read ClusterVirtualNetworkBaseline + the per-NS VirtualNetworkBaseline from cache).
    //    Build BindingRules (List VirtualNetworkBinding in pod.Namespace, filter by selector match).
    //    Build PodLabelRules (parse pod.Labels for kube-vnet/net.* keys, canonicalize).
    // 4. Call Resolve(rules) — same pure function ResolutionReconciler uses (resolution.go).
    // 5. Compute desired system labels (kube-vnet.system/net.<homeNS>.<vnet>=<dir>) + the
    //    resolved-generation annotation + resolved-by=admission annotation.
    // 6. Diff desired vs pod's current system labels. If no diff, return Allowed (unmodified).
    //    If diff, build a JSON Patch (add/replace operations) and return PatchResponse.
}
```

Key design points:
- **No I/O** beyond cache reads. The cache is already populated by the controller manager (the webhook server shares the same manager). Cache reads are sub-millisecond.
- **Pure function reuse**: `Resolve()` is already pure and unit-tested. The handler is glue (decode → fetch rules from cache → call Resolve → encode patch). Most logic comes from the existing resolution loop in `internal/controller/resolution_controller.go` — refactor the rule-building helpers (`baselineRules`, `bindingRules`, `podLabelRules`) to take a `client.Reader` instead of a `client.Client` so both the webhook handler and the controller can call them. They already only do reads.
- **Patch shape**: JSON Patch (`application/json-patch+json`), one `add`/`replace` op per label key + the two annotations. Strategic merge patch is also fine; JSON Patch is more deterministic for testing.

### Failure semantics

`failurePolicy: Ignore` is the load-bearing choice. The baseline is the security boundary, not the webhook. If the webhook is unreachable or errors:

- Admission proceeds.
- The pod lands without system labels.
- The baseline selects it (label-absent matches `NotIn`).
- Deny-all applies. **Fail-closed.**
- The `ResolutionReconciler` then stamps the labels asynchronously, just like today.

The webhook is an *acceleration*, not a gate. Choosing `Fail` would mean a webhook outage blocks all pod creation cluster-wide — catastrophic. `Ignore` keeps the webhook safe to enable.

### Restart safety

The webhook is opt-in via the chart flag `webhook.enabled` (default `false`). When enabled, `objectSelector` and `namespaceSelector` exclude:

- The operator's own pods (label `app.kubernetes.io/name=kube-vnet`) — prevents the operator pod admission from depending on an operator pod that may not be running yet.
- `kube-system`, `kube-public`, `kube-node-lease`, and the operator's release namespace — same reasons the operator already excludes them via `--disabled-namespaces`.
- Any namespace annotated `kube-vnet/disabled=true`.

Defense in depth: both selectors apply.

### Coexistence with `ResolutionReconciler`

The reconciler still runs and is still authoritative for non-admission resolution events:

- Cluster/namespace baseline edits (force re-resolution of every affected pod).
- Binding spec changes (re-evaluate which pods the binding selects).
- Namespace managed-status flips.
- Operator restart catch-up (pods admitted while the webhook was disabled or down).

On a pod CREATE that the webhook stamped, the reconciler's first reconcile is a no-op (desired-vs-actual diff is empty). Both produce the same labels because both call the same `Resolve()` against the same cache.

### Marker annotations

`kube-vnet.system/resolved-generation` is set by the webhook to the pod's upcoming generation (always 1 for CREATE; for UPDATE, `pod.Generation` is the post-mutation value the apiserver will assign — approximate by setting to the request's `metadata.generation` if present, else 1).

A sibling annotation `kube-vnet.system/resolved-by=admission` (vs `=controller`) lets debugging distinguish which path stamped. The reconciler overwrites both annotations when it re-resolves; the marker is purely diagnostic.

### VAP exemption

The existing `ValidatingAdmissionPolicy` that protects `kube-vnet.system/*` labels from user mutation needs to permit the operator's ServiceAccount (the webhook's identity). The operator is already exempt because it bypasses VAPs by design (system-managed); confirm the exemption applies to webhook writes by serviceaccount identity, not by code path.

### Cert lifecycle

Two supported sources, configurable via `webhook.certSource`:

- `helm` (default): self-signed CA + serving cert generated by Helm's `genCA` / `genSignedCert` template functions at install time. Stored in a Secret. On `helm upgrade`, the existing Secret is reused via `lookup` (no re-rotation unless missing or expired). Zero external dependencies. Acceptable for the opt-in audience.
- `cert-manager`: chart renders a `Certificate` resource against an `Issuer` the user provides. Recommended for clusters that already run cert-manager.
- (Implicit third option: `external` — user provides the Secret out-of-band; chart only renders the `MutatingWebhookConfiguration` and trusts the named Secret to exist.)

## Consequences

- **Resolution race window collapses to zero on the admission path** for both CREATE and UPDATE. Liveness gap closed for autoscaled latency-sensitive services.
- **Security posture unchanged.** The baseline remains the boundary; the webhook is an acceleration. `failurePolicy: Ignore` ensures a webhook outage doesn't block pod creation and doesn't weaken security.
- **No new RBAC.** The handler reads through the controller-runtime cache; no new resource types accessed.
- **Cache reuse.** The webhook server runs in the same controller-runtime manager process; informers are already populated. Sub-millisecond resolution latency.
- **Sender-side visibility gap is partially closed**: peer-membership policies still depend on the controller's reconcile to refresh `from:` rules naming the new pod's NS. The webhook closes the *receiver* side (new pod has correct labels at admission); the sender side remains a second reconcile window, additive on top of the now-zero first window. A future enhancement could pre-stamp `kube-vnet.system/network=...`-labeled NetworkPolicies' `from:` rules at admission, but that's a much larger change (admission-time policy mutation, not just pod mutation) and out of scope here.
- **Operator pod admission cycle**: the operator's own pods are excluded via `objectSelector`, so an operator restart doesn't deadlock on its own webhook. New operator pods admit unstamped (handled by their normal lifecycle — operator pods don't need vnet membership themselves).
- **Diagnostic clarity**: `kube-vnet.system/resolved-by=<admission|controller>` annotation lets users see which path stamped each pod. Useful when debugging webhook coverage gaps.

## Alternatives considered

- **`ValidatingAdmissionPolicy` + post-admission Patch.** Rejected: VAPs can't mutate. We could enforce label correctness at admission with a VAP and let the controller patch asynchronously, but that doesn't close the race window.
- **Init-container approach.** Rejected: requires pod-template changes; can't intercept third-party charts; adds container start latency to every pod.
- **Skip the webhook entirely (current state).** Fine for most workloads — the baseline keeps the cluster safe. Fails for autoscaled latency-sensitive services where the ~100ms window manifests as user-visible request failures.
- **`failurePolicy: Fail`.** Rejected: a webhook outage would block all pod creation cluster-wide, including the operator's own restart and any DaemonSet that needs to land. Not worth the operational fragility for an acceleration that's already covered by a fail-closed safety net.
- **Replace the `ResolutionReconciler` with the webhook.** Rejected: the reconciler is still needed for resolution events that don't go through pod admission (baseline edits, binding spec changes, namespace flips, restart catch-up). Webhook augments, not replaces.
- **Pre-stamping `VirtualNetworkBinding.status.AttachedPods` at admission.** Out of scope. Status fields are reconciler territory; the webhook only mutates the incoming pod.

## Open design choices (to confirm at implementation time)

1. **Default `webhook.enabled` value** — recommend `false` for the first release (opt-in), flip to `true` after one or two release cycles of real-world soak. Alternative: `true` immediately, on the bet that `failurePolicy: Ignore` makes it safe-by-default.
2. **Default `webhook.certSource`** — recommend `helm` (self-signed bake-in via `genCA`/`genSignedCert`) so installation needs no extra dependencies. Document `cert-manager` as the supported alternative.
3. **`reinvocationPolicy: IfNeeded`** — needed if other webhooks mutate `kube-vnet/net.*` pod labels after ours runs (unlikely but possible for templated workloads with downstream mutators). Cheap to enable; recommend on.
4. **Should the webhook also accept `VirtualNetworkBinding` CREATE/UPDATE events?** Bindings affect resolution of *existing* pods, and admission-time stamping doesn't help there (pods are already admitted). Recommend leaving bindings to the controller; webhook stays pod-only.

## Out of scope

- Pre-stamping `from:` rules in peer membership policies at admission time. Would require admission-time NetworkPolicy mutation, much larger scope.
- Replacing the `ResolutionReconciler` with the webhook. Reconciler remains authoritative for non-admission resolution events.
- A `ValidatingAdmissionWebhook` to enforce that user-supplied `kube-vnet/net.*` labels reference real vnets at admission. Today the controller surfaces these as Degraded reasons (`BareJoinLabelVnetNotFound`, `PrefixedJoinLabelVnetNotFound`); converting to admission-time rejection is a separate decision worth its own ADR.
- Cluster-scoped resources (Namespace, etc.). Webhook is pod-only.
- Renaming the `kube-vnet.system/resolved-generation` annotation. Folded into the existing follow-up thread.
