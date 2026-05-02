# 0024 — Operator ingress-isolation default + per-mode override lists

Status: Accepted

## Context

[ADR 0020](0020-default-deny-unmanaged-namespaces.md) shipped the boolean `--default-deny-everywhere`: a single switch that flipped the cluster-wide baseline policy from off to on. With the introduction of the three-valued `ingress-isolation` enum in ADRs 0023 + 0025, a single boolean is no longer expressive enough.

The user wants:

- A cluster-wide default mode for namespaces that don't explicitly override.
- Per-mode override lists naming specific namespaces that should be forced into one mode regardless of the default.
- Compat with the previous `--default-deny-everywhere` flag during a deprecation window.

## Decision

Four flags replace the old boolean:

| Flag | Type | Default | Description |
|---|---|---|---|
| `--ingress-isolation` | string | `none` | Cluster-wide default isolation mode for namespaces that don't override. |
| `--ingress-isolation-none` | string (CSV) | `""` | Namespaces forced to `none` regardless of the default. |
| `--ingress-isolation-namespace` | string (CSV) | `""` | Namespaces forced to `namespace`. |
| `--ingress-isolation-pod` | string (CSV) | `""` | Namespaces forced to `pod`. |

### Per-namespace resolution (highest to lowest precedence)

1. `kube-vnet/ingress-isolation` annotation on the `Namespace` if set to a recognized value.
2. If the namespace name appears in any of `--ingress-isolation-{none,namespace,pod}`, the matching mode wins.
3. Otherwise, `--ingress-isolation` (the cluster-wide default).

A namespace appearing in more than one of the three override lists is a configuration error: the operator logs an error and refuses to start. Inside-list duplicates are deduplicated silently.

Excluded namespaces (`--excluded-namespaces` flag or `kube-vnet/disabled=true` annotation) are not subject to any of this — the operator does nothing in them.

### Helm value mirror

```yaml
operator:
  ingressIsolation:
    mode: none                 # none | namespace | pod
    forceNone: []
    forceNamespace: []
    forcePod: []
```

(camelCase per the Helm convention; matches `operator.labelPrefix`, `operator.leaderElect`, etc.)

### Backward compatibility

`--default-deny-everywhere=true` is still accepted. When set:

- If `--ingress-isolation` is at its default `none`, the operator treats the deprecated flag as `--ingress-isolation=pod` and logs a deprecation warning at startup.
- If `--ingress-isolation` was set to anything explicit, the new flag wins and the operator logs that the deprecated flag is being ignored. Same semantics in the chart's `operator.defaultDenyEverywhere`.

The deprecated flag will be removed in a future release.

### Why three override flags rather than a single mode + except list

Three flags are slightly more verbose but unambiguous: each list says exactly what mode it forces. A single "mode + except" form (e.g. `--ingress-isolation-mode=on --ingress-isolation-except=ns1,ns2`) requires the reader to know which mode "except" flips to, which is one more thing to look up. With three explicit lists the answer is right there.

The startup-time conflict check catches the "namespace in two lists" mistake at the earliest possible moment, with a clear error message.

## Consequences

- **Pro**: Three modes are first-class instead of a binary. Same expressive power as the per-namespace annotation.
- **Pro**: Override lists let cluster operators carve out exceptions to the default without forcing every namespace owner to add an annotation.
- **Pro**: Backward-compatible. Existing installs with `--default-deny-everywhere=true` continue to work, with a deprecation warning.
- **Con**: Four flags instead of one. Mitigated by the fact that most installs only set the cluster-wide default.
- **Con**: Two ways to express the same thing (operator-level override list vs per-namespace annotation). The precedence rule (annotation wins) handles the conflict.

## Cross-references

- ADR 0020 — `--default-deny-everywhere`. Superseded by this ADR for the operator-level config rename.
- ADR 0023 — decoupled `disabled` and `ingress-isolation`. The annotation half of the same redesign.
- ADR 0025 — `ingress-isolation` rename + egress unrestricted. The shape of what the mode controls.

## Addendum (2026-05-02)

The chart's per-mode override keys have been renamed from `operator.ingressIsolation.{forceNone,forceNamespace,forcePod}` to `operator.ingressIsolation.namespaceOverrides.{none,namespace,pod}`. The new keys nest naturally under `ingressIsolation` and read the same way the resolution rule reads ("override to mode `none`", etc.). The old keys are accepted for one release with a deprecation warning rendered through `NOTES.txt`. CLI flag names are unchanged — only the chart-side keys moved.

`operator.ingressIsolation.mode` is now required on the chart side: there is no default, and `helm install` / `helm upgrade` fails fast via `required` if the value is empty. The operator binary mirrors this — it exits non-zero at startup if `--ingress-isolation` was not set explicitly. `--default-deny-everywhere=true` continues to satisfy the requirement (mapping to `pod` with a deprecation warning) for one cycle.

The chart's default for `namespaceOverrides.none` is `[kube-system, kube-public, kube-node-lease]`, and the operator's `--ingress-isolation-none` flag default is the same CSV. These three namespaces have correspondingly been removed from the chart's default `disabledNamespaces` (formerly `excludedNamespaces`) and from the operator's `--disabled-namespaces` flag default — the operator now *discovers* deliberate joiner pods in those namespaces but never installs an ingress baseline there. The implicit `POD_NAMESPACE` self-inclusion in disabled-namespaces is unchanged.

The deprecated aliases described above (`operator.ingressIsolation.{forceNone,forceNamespace,forcePod}`, `operator.excludedNamespaces` / `--excluded-namespaces`, `operator.defaultDenyEverywhere` / `--default-deny-everywhere`) were removed in a follow-up commit shortly after this addendum was written. The one-cycle compatibility window described above did not actually ship in any tagged release — the renames and removals landed in the same `v0.1.0` cut. Installs migrating from local builds of earlier `main` must rename their config before upgrading.
