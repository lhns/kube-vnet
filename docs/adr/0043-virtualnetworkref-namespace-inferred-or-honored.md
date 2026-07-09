# ADR 0043 — `virtualNetworkRef.namespace`: optional, inferred, or honored — never ignored

**Status**: Accepted (2026-07-09)

## Context

`VirtualNetworkRef` carries `{name, namespace}`. Until now, `namespace` was a **required field that resolution silently discarded** for the two reserved system-vnet names:

```go
// resolution_controller.go, before
if ref.Name == SystemVnetCluster   { return VnetKey(SystemVnetCluster) }          // ref.Namespace ignored
if ref.Name == SystemVnetNamespace { return VnetKey(podNS + "." + SystemVnetNamespace) } // ref.Namespace ignored
return VnetKey(ref.Namespace + "." + ref.Name)                                    // user vnets: honored
```

Two things followed, both bad:

1. **Any value worked.** `{name: namespace, namespace: kube-vnet-system}` was rewritten to `<podNS>.namespace` and joined the pod's *own* namespace vnet. The written namespace was decorative — including namespaces that hold no such vnet.
2. **The docs asserted the fiction.** The chart's seeded `ClusterVirtualNetworkBaseline` rendered `namespace: {{ .Release.Namespace }}` for bare system keys, and `values.yaml` / `configuration.md` / the template header all claimed system vnets "live in the operator's release namespace". That is true for `cluster` and **false for `namespace`**: `SystemVnetReconciler` creates a `namespace` vnet in every *managed* namespace, and the release namespace is implicitly disabled (`POD_NAMESPACE` is appended to `disabledNamespaces`), so it has none.

The failure mode this produced: a `VirtualNetworkBaseline` pointing at a vnet that does not exist appeared to work perfectly, and nobody could explain why. A user who reasoned correctly about the model concluded it *shouldn't* work — and was right.

Separately, `Permits` short-circuited on the vnet **name**:

```go
if vnetName == SystemVnetCluster { return true, nil }   // homeNS never inspected
```

so `bogus.cluster` was permitted. The cluster vnet's wrong namespace could not be caught even in principle.

## Decision

`namespace` becomes **optional**. Omitting it is the recommended form. When present it is **honored verbatim** and never rewritten.

| `name` | omitted → inferred as | explicit | wrong value |
|---|---|---|---|
| `cluster` | the cluster-wide singleton (bare key) | honored, fully qualified | not-found → not permitted |
| `namespace` | the pod's own namespace | honored | denied by that vnet's own join permissions |
| user vnet | the pod's own namespace | honored (cross-NS joins stay legal) | not-found / not permitted |

### No vnet kind is special-cased

A wrong namespace is **not** a validation error, and not an admission concern. It simply names a vnet the pod cannot join, and is denied by the ordinary permission path:

- `{name: namespace, namespace: other-ns}` is a coherent *attempt* to join `other-ns`'s namespace vnet. It fails only because system namespace-vnets set no `spec.allowedNamespaces` — precisely as a user vnet that doesn't allow you would fail. Nothing about `namespace` is hard-coded.
- `{name: namespace, namespace: kube-vnet-system}` names a vnet that doesn't exist → not-found.
- `{name: cluster, namespace: bogus}` names `bogus.cluster` → not-found. The real cluster vnet lives in the operator's namespace with `allowedNamespaces: {All: true}`, so a correct ref permits naturally. **The singleton's home is discovered by the `Get`, never hardcoded** — resolution does not need to know the operator's namespace.

`canonicalVnetKey` is therefore pure inference: total, no error return, no per-kind branch beyond choosing what to infer when the field is absent.

### The one real bug was in `Permits`

Uniformity required fixing the name-only short-circuit, not adding a guard for `cluster`. `splitVnetKey` already yields `homeNS == ""` for the bare canonical form, so:

```go
if vnetName == SystemVnetCluster && homeNS == "" { return true, nil } // bare canonical form only
// falls through:  <operatorNS>.cluster → exists, All:true → permitted
//                 bogus.cluster        → NotFound        → not permitted
```

### Permission on the qualified key; identity in canonical form

An explicit `<ns>.cluster` must stay qualified long enough for `Permits` to verify it, but ADR 0033 mandates the bare `cluster` key for pod stamps and policy names. So the order is: **check permission on the fully-qualified key, then collapse to the canonical form** (`CanonicalSuffix`, which maps `<anything>.cluster` → `cluster`) for surviving rules only.

### Diagnostics: one mechanism, one reason, richer message

`filterPermittedRules` previously dropped non-permitted rules **silently**. It now emits a Warning Event on the object that declared the rule (Baseline, Binding, or the Pod for join labels).

- The **reason is uniform** — `VirtualNetworkNotJoinable` for every vnet kind. It is the machine contract that alerts and `--field-selector` key on; branching it would reintroduce the special-casing this ADR removes.
- The **message is enriched** by `notJoinableHint(ref)`, a *pure formatting* helper that adds a targeted suggestion for the two reserved names. It must never influence control flow.
- The note distinguishes the two genuinely different failures — *"VirtualNetwork X does not exist in namespace Y"* vs *"exists but does not permit namespace Y"* — since they have different fixes.

### Why not CEL / admission

CEL cannot know the operator's release namespace, and the value inferred for `namespace` depends on the *pod being resolved* — a `ClusterVirtualNetworkBaseline` is cluster-scoped and resolves per-pod, so there is no single correct value to validate at admission. Enforcement belongs exactly where every other "can this pod join this vnet?" decision already lives: `Permits`, at resolution time.

A consequence worth stating: in a **cluster-scoped** `ClusterVirtualNetworkBaseline`, the only sensible `namespace` for the `namespace` vnet is *omitted*. Any explicit value would be correct for pods in that one namespace and wrong for every other pod.

## Migration

None, deliberately. Existing clusters carry a seeded `ClusterVirtualNetworkBaseline` with `{name: namespace, namespace: <release-ns>}` — the chart wrote it. After upgrade the operator honors that ref, finds no such vnet, drops the rule, and says so via the Event. The chart **self-heals**: the CVNB is a `post-install,post-upgrade` hook, so `helm upgrade` re-renders it without the attribute and the posture reconverges. (The hook must remain `post-*`; a `pre-*` hook would apply the namespace-less CVNB against the *old* CRD, which still requires the field.)

Expect a brief window during the roll where the cluster-baseline `namespace` membership is absent. Kustomize users and hand-written baselines/bindings that set an explicit system-vnet namespace will get the Warning Event and a dropped rule until they omit the field — the intended behavior, not a regression. The fix is one line: delete `namespace:` from the system-vnet ref.

## Consequences

- **Positive**: the API no longer lies. A ref means what it says; a wrong ref fails visibly instead of silently succeeding.
- **Positive**: `namespace` is optional, so the common cases (`cluster`, the local `namespace` vnet, a same-namespace user vnet) need no namespace at all.
- **Positive**: user vnets gained the same inference — `{name: web}` in a Binding means the local `web` vnet.
- **Positive**: the whole class of silent membership drops is now observable (`VirtualNetworkNotJoinable`), closing a long-standing gap flagged by a `// See ADR (TODO)` in `filterPermittedRules`.
- **Negative**: a behavior change for anyone who wrote the release namespace onto a `namespace` ref because our docs told them to. Self-healing via Helm; a one-line fix otherwise.
- **Negative**: `Permits` now performs one extra `Get` for a qualified `<ns>.cluster` key. Bare `cluster` — the form every stamp and policy uses — still short-circuits.

## References

- [ADR 0030 — Unified vnet-membership model](0030-unified-vnet-membership-with-resolution.md) (system vnets; `SystemVnetReconciler`)
- [ADR 0031 — Baseline tier resolution](0031-baseline-tier-resolution.md) (baselines and the `<ns>.<name>` key convention)
- [ADR 0033 — Canonical FQ system labels](0033-canonical-fq-system-labels.md) (bare `cluster` canonical form; `CanonicalSuffix`)
