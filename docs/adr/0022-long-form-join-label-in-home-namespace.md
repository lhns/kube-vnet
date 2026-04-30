# 0022 — Long-form join label accepted in the home namespace

Status: Accepted

## Context

The original join-label encoding (ADR 0004) gave home-namespace pods the **bare** form (`kube-vnet/net.<vnet>`) and foreign-namespace pods the **prefixed** form (`kube-vnet/net.<homeNS>.<vnet>`). Convenient when each Deployment's namespace is fixed, awkward when the same template is deployed into multiple namespaces — e.g., a Helm chart that's installed once into the vnet's home namespace and again into a foreign namespace would need different label keys per install.

## Decision

A pod in the **home namespace** is recognized as a member if it carries **either** the bare or the prefixed form of the join label. (Foreign-namespace pods continue to require the prefixed form — the bare form would be ambiguous there since "this namespace" doesn't necessarily equal the vnet's home.)

### Conflict handling

If a home-namespace pod carries both forms simultaneously, the operator inspects the parsed `Direction` values:

- Both present and identical → one member, no conflict.
- Both present and different (e.g. `kube-vnet/net.X=both` and `kube-vnet/net.<homeNS>.X=ingress`) → `Degraded`/`InvalidJoiners` reason `ConflictingDirections`. The pod is excluded from membership.
- One present and the other reads as `none` (i.e., `false`) → `Degraded`/`ConflictingDirections`. The user explicitly opted out via one form and in via the other; ambiguous intent.

### Generated policy

Each direction class can now require **two** policies per (vnet, home namespace) — one matching the bare key, one matching the prefixed key. NetworkPolicy `podSelector` is a single `LabelSelector` (no OR), so we can't combine the two forms into one policy.

Naming:

| Form | Direction | Policy name |
|---|---|---|
| Bare | bidi | `kube-vnet-<vnet>-<ns>` (legacy unsuffixed) |
| Bare | ingress | `kube-vnet-<vnet>-<ns>-ingress` |
| Bare | egress | `kube-vnet-<vnet>-<ns>-egress` |
| Prefixed | bidi | `kube-vnet-<vnet>-<ns>-prefixed` |
| Prefixed | ingress | `kube-vnet-<vnet>-<ns>-ingress-prefixed` |
| Prefixed | egress | `kube-vnet-<vnet>-<ns>-egress-prefixed` |

Up to 6 policies in the home namespace if both forms and all three direction classes are in use. Most installs see one or two — the common case is "everyone uses the bare form" (1 policy) or "everyone uses the prefixed form because it's templated" (1 policy with `-prefixed` suffix).

When the policy name's deterministic form exceeds 253 characters, truncation + sha256 suffix applies (per ADR 0011).

### Discovery and peer rules

`discoverMembers` looks for both forms when iterating home-namespace pods. The output includes a per-form record so the generator knows which forms to materialize.

Peer rules referencing the home namespace include separate `from`/`to` entries per (form, direction-class) combination that has at least one matching peer. Multiple peer entries are OR'd by NetworkPolicy semantics, so this is correct without additional policy growth on the consuming side.

## What we explicitly didn't do

- **Mutate user pod labels.** The natural shortcut of "if a home pod has only the prefixed form, the operator adds the bare form too" was rejected. The operator never writes to user resources.
- **Generate one policy with two podSelectors.** NetworkPolicy doesn't support that. Two policies is the only correct shape.
- **Switch the home-namespace canonical form to prefixed.** That would force every existing user to rewrite their bare-form labels. Both forms work; bare stays canonical for the home namespace (matches what every existing manifest assumes).

## Consequences

- **Pro**: Templated workloads can use a fixed (prefixed) label across namespaces.
- **Pro**: Backward-compatible. Every existing bare-form manifest continues to work and produces the same policy name.
- **Con**: Up to 2× more policies in the home namespace when both forms are in use. Minor at typical scales.
- **Con**: Conflicts between the two forms' direction values surface as Degraded — users mixing the forms inconsistently see the error in `kubectl describe vnet`.

## Cross-references

- ADR 0004 — Bare vs namespace-prefixed join label. This ADR loosens the "bare is the only form in home" rule.
- ADR 0011 — Policy naming and truncation. Same hash-suffix mechanism applies to the new direction/form-suffixed names.
- ADR 0021 — Direction modes on join labels. The two-policy-per-form shape interacts with the per-direction-class shape: in the worst case, 6 policies in the home namespace.
