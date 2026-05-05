# 0023 — Decoupled `disabled` and `ingress-isolation` namespace annotations

Status: Superseded by [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (2026-05-05). The decoupling principle (separate "operator stays out entirely" from "operator manages but doesn't isolate") is preserved in the new model: `--disabled-namespaces` survives as a privilege-boundary allowlist; the per-namespace `kube-vnet/disabled=true` annotation also survives. What changed is that the per-namespace `kube-vnet/ingress-isolation` annotation is gone — its job is now done by the inheritance lattice over system vnet defaults.

## Context

[ADR 0006](0006-baseline-default-deny-and-single-opt-out.md) collapsed two distinct namespace concerns into a single annotation `kube-vnet/disabled`:

- "The operator should not generate any membership policies for pods in this namespace." (membership opt-out)
- "The operator should not install the deny baseline in this namespace." (baseline opt-out)

The bundling looked clean ("the baseline *is* the operator being enabled"), but it forecloses a real use case: a namespace owner who wants their pods to be members of cross-namespace vnets *without* taking on a deny-default posture for unjoined pods. Under ADR 0006 they'd have to either accept the baseline or lose all kube-vnet functionality. There's no middle ground.

[ADR 0006] also coupled baseline existence to membership presence: "in any namespace where at least one pod joins a VirtualNetwork, the operator ensures `kube-vnet-default-deny` exists." This implicit "first member triggers the baseline" rule is surprising — it means the baseline appears as a side effect of an unrelated action.

## Decision

Two **independent** annotations:

| Annotation | Default | Effect |
|---|---|---|
| `kube-vnet/disabled=true` | absent → false | Operator does nothing in this namespace. No membership policies, no baseline, pods here are not eligible peers in any vnet, bindings here are ignored. |
| `kube-vnet/ingress-isolation=none\|namespace\|pod` | absent → operator default | Sets the baseline mode for this namespace. Independent of `disabled` (ignored when `disabled=true` because the operator is inert there) and independent of vnet membership presence. |

The "first member triggers the baseline" coupling is **gone**. Baseline existence is decided purely by the resolved ingress-isolation mode. A namespace can have full vnet membership and `ingress-isolation=none` (no baseline) — useful for "make my pods reachable on the network but I'll handle isolation with my own policies."

### Effective behavior matrix (when not `disabled`)

| `ingress-isolation` resolved | Has vnet members | Result |
|---|---|---|
| `pod` | yes | membership policies + strict ingress baseline |
| `pod` | no | strict ingress baseline only |
| `namespace` | yes | membership policies + same-namespace-ingress baseline |
| `namespace` | no | same-namespace-ingress baseline only |
| `none` | yes | membership policies only — ingress restricted only by other policies |
| `none` | no | nothing — namespace is unmanaged from a baseline perspective |

`disabled=true` overrides everything: nothing kube-vnet-related happens in that namespace.

### Ownership change

The membership-driven baseline-install code path is **removed** from `VirtualNetworkReconciler`. `NamespaceReconciler` becomes the sole owner of the baseline lifecycle. It watches `Namespace` events and applies / removes the baseline based on the resolved `ResolveIsolation(ns)`.

This simplifies both reconcilers:

- `VirtualNetworkReconciler` is purely about membership policies. It never touches baselines.
- `NamespaceReconciler` is purely about baselines. It never inspects vnets.

The "two reconcilers writing the same baseline" coordination problem from ADR 0020 disappears — only one reconciler writes the baseline now.

## Consequences

- **Pro**: Three modes (`none` / `namespace` / `pod`) instead of a binary. Real expressiveness for users who want to mix membership with non-default-deny postures.
- **Pro**: Simpler reconciler split. Each reconciler has a clear, narrow ownership.
- **Pro**: Baseline behavior is predictable and explicit. No "side-effect" creation from membership changes.
- **Con**: **Behavior change**. Existing installs that relied on the "first member triggers the baseline" rule will lose their baseline unless they explicitly set `kube-vnet/ingress-isolation=pod` (or use the operator-level config — see ADR 0024). CHANGELOG and the upgrade docs flag this loudly.
- **Con**: Two annotations to learn instead of one. Mitigated by docs and by the orthogonality being intuitive once explained.

## Cross-references

- ADR 0006 — single per-namespace opt-out. Superseded by this one.
- ADR 0020 — `--default-deny-everywhere` flag. Superseded by ADR 0024 (which renames and extends the operator config to match the new annotation).
- ADR 0025 — `ingress-isolation` rename + ingress-only scope (and the egress-unrestricted behavior change). Companion to this ADR.
