# 0020 — `--default-deny-everywhere` flag for cluster-wide default-deny

Status: Accepted

## Context

Two distinct operating models for kube-vnet make sense in different situations:

**Opt-in (per-namespace).** Default since v1alpha1: the operator does nothing in a namespace until at least one pod joins a VirtualNetwork there. The first member triggers the `kube-vnet-default-deny` baseline. Namespaces with no membership stay default-allow, indistinguishable from a vanilla cluster. Installing the operator on an existing cluster has zero immediate effect on namespaces that don't use it.

**Opt-out (cluster-wide).** Cluster operator wants kube-vnet to *be* the cluster's network-policy posture: every namespace deny-by-default; pods opt into specific connectivity by joining VirtualNetworks. This is the more secure default and more useful as a cluster-level control, but switching to it against an existing cluster breaks every workload that previously relied on default-allow until it's covered by a VirtualNetwork.

Both modes are valid. The trade-off is between greenfield safety (opt-out is the right default for a new cluster) and migration friction (opt-in keeps existing workloads functional during rollout).

## Decision

Add the flag `--default-deny-everywhere`, default `false`. When `true`, a new `NamespaceReconciler` watches `corev1.Namespace` events and ensures the `kube-vnet-default-deny` baseline in every namespace that:

- is **not** in the operator-level exclusion list (`--excluded-namespaces`), and
- does **not** carry the `kube-vnet/disabled=true` annotation.

Removal is symmetric: a namespace gaining the disabled annotation, or being added to the exclusion list, or the flag being turned off, has its baseline GC'd — provided no operator-managed membership policy still references it (the membership-driven baseline always wins).

The reconciler short-circuits when the flag is `false`, so today's behavior is byte-identical for users who don't opt in.

The two ownership paths produce identical baseline policies (same name `kube-vnet-default-deny`, same labels), so server-side apply with field manager `kube-vnet` reconciles their writes idempotently.

### Naming

The flag is `--default-deny-everywhere`. Earlier names considered and rejected:

- `--default-deny-all-managed-namespaces` — confusing because "managed" means different things in different parts of the codebase.
- `--default-deny-unmanaged-namespaces` — technically the flag *covers* namespaces not currently managed by any vnet membership, but "unmanaged" reads as "the operator ignores them," which is the opposite of what the flag does.
- `--cluster-default-deny` — could be confused with `AdminNetworkPolicy`, which is genuinely cluster-scoped (see ADR 0019).

`--default-deny-everywhere` pairs naturally with the existing escape valves: "everywhere" describes the intent; `--excluded-namespaces` and `kube-vnet/disabled` describe what's exempted from the everywhere.

## Consequences

- **Pro**: Lets a cluster operator turn kube-vnet into the cluster's primary network-policy posture in one switch.
- **Pro**: Backwards-compatible: default `false` means no behavior change for existing users.
- **Pro**: The escape valves (excluded list, disabled annotation) work identically in both modes — same conceptual model.
- **Pro**: Per-vnet membership baselines and flag-driven baselines coexist cleanly via SSA + same name + same labels.
- **Con**: Two reconcilers can both write the baseline. SSA makes this safe but adds a small mental-model cost; documented here.
- **Con**: Flipping the flag on against an existing cluster is a substantial behavior change. Documented in the README's migration section. Default stays opt-in to make accidents unlikely.
- **Con**: With the flag on, namespaces that *should* have always-allow (e.g. some test namespaces) need the `kube-vnet/disabled=true` annotation. Operationally fine but worth noting.

## Cross-references

- ADR 0006 — single per-namespace opt-out via `kube-vnet/disabled=true`. Same annotation works as the escape valve in this mode.
- ADR 0007 — operator-level `--excluded-namespaces`. Also acts as an escape valve in this mode.
- ADR 0014 — deferred items.
- ADR 0019 — baseline durability. With this flag on, every namespace's baseline is subject to the same drift-correction defense.
