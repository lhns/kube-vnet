# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the API surface is `v1alpha1`, breaking changes are possible across any
release. Pinning to an exact version is recommended.

## [Unreleased]

### Breaking

- **`operator.ingressIsolation.mode` (Helm) and `--ingress-isolation` (CLI) are
  now required.** The chart no longer ships a default for `mode`; `helm install`
  and `helm upgrade` fail fast via `required` if it's empty. The operator
  binary exits non-zero at startup if `--ingress-isolation` is not set
  explicitly. Existing chart users: add `mode: <none|namespace|pod>` to your
  values (or pass `--set operator.ingressIsolation.mode=...`) before
  upgrading. Operator-binary users: add `--ingress-isolation=...` to your
  manager args. The deprecated `--default-deny-everywhere=true` /
  `operator.defaultDenyEverywhere=true` continues to satisfy the requirement
  (mapping to `pod` with a deprecation warning) for this cycle only.

### Behavior changes (upgrade-impacting)

- **`kube-system`, `kube-public`, and `kube-node-lease` are no longer in
  `disabledNamespaces` (formerly `excludedNamespaces`) by default.** They are
  now in `operator.ingressIsolation.namespaceOverrides.none` (chart default
  `[kube-system, kube-public, kube-node-lease]`; the operator's
  `--ingress-isolation-none` default is the same CSV). The operator now
  *discovers* deliberate joiner pods in those namespaces (so a cluster admin
  can enroll a kube-system component in a vnet via the prefixed join label
  and `allowedNamespaces`), but never installs an ingress-deny baseline
  there, regardless of the cluster-wide `mode`. If you relied on the
  operator never touching those namespaces *at all*, add them back to
  `operator.disabledNamespaces` explicitly. The implicit `POD_NAMESPACE`
  self-inclusion in `disabledNamespaces` is unchanged.

- **The operator no longer restricts egress.** The baseline now carries
  `policyTypes: [Ingress]` only. Membership policies still allow egress to
  vnet peers, but generic egress (DNS, the apiserver, the public internet,
  other namespaces) is unrestricted by kube-vnet. **Existing installs will
  see their egress posture loosen on upgrade.** If you need per-workload
  egress restriction, write a user-managed `NetworkPolicy` with
  `policyTypes: [Egress]`. See ADR 0025 and `docs/security.md`.
- **The implicit "first vnet member triggers the baseline" coupling is
  gone.** Baseline existence is now decided purely by the resolved
  `ingress-isolation` mode for the namespace. To preserve the previous
  behavior, set `kube-vnet/ingress-isolation: pod` on each namespace
  explicitly (or use `--ingress-isolation=pod` cluster-wide). See ADR 0023.

### Added

- **Direction modes on join labels.** The label value declares the pod's
  participation: `both` (default), `ingress`, `egress`, `none`. Legacy
  aliases: `"true"` → `both`, `"false"` → `none`. Per-direction policies
  are emitted (`-ingress` / `-egress` suffixes); the unsuffixed name keeps
  the legacy form for the bidirectional case. Unknown values surface as
  `Degraded`/`UnknownDirection`. See ADR 0021.
- **Long-form join label accepted in the home namespace.** Both
  `kube-vnet/net.<vnet>` and `kube-vnet/net.<homeNS>.<vnet>` work in the
  home namespace, useful for templated workloads. Conflicting directions
  across the two forms surface as `Degraded`/`ConflictingDirections`.
  See ADR 0022.
- **`kube-vnet/ingress-isolation` namespace annotation.** Three values:
  `none` (no baseline), `namespace` (baseline allows ingress from
  same-namespace pods), `pod` (strict ingress deny). Independent of
  `kube-vnet/disabled` (which still turns the operator off entirely for
  that namespace). See ADR 0023.
- **`--ingress-isolation` flag family.** Cluster-wide default mode plus
  three per-mode override CSV lists (`--ingress-isolation-none`,
  `--ingress-isolation-namespace`, `--ingress-isolation-pod`). Helm:
  `operator.ingressIsolation.{mode,namespaceOverrides.{none,namespace,pod}}`.
  Per-namespace annotation > override list > cluster-wide default. See
  ADR 0024.
- **`VirtualNetworkBinding` CRD** (short names `vnb`, `vnbs`). Namespaced.
  Selects pods *in its own namespace* via `spec.podSelector` and attaches
  them to a target vnet (`spec.virtualNetworkRef.{name,namespace}`) for a
  chosen `direction` (default `both`). The escape hatch for enrolling
  pods whose template you can't modify (third-party Helm charts, pods
  owned by another operator). The target vnet's `spec.allowedNamespaces`
  is enforced. Status: `Ready` condition with reasons `PodsAttached`,
  `NoPodsMatch`, `VirtualNetworkNotFound`, `NamespaceNotAllowed`,
  `NamespaceExcluded`, `UnknownDirection`, `InvalidSelector`; plus
  `attachedPods` and `observedGeneration`. Per-binding policies are
  named `kube-vnet-<vnet>-b-<binding>` and labeled
  `kube-vnet/binding=<binding>`. See ADR 0026.
- Helm chart at `charts/kube-vnet`, published as an OCI artifact to
  `ghcr.io/lhns/charts/kube-vnet` on every release tag. Install via
  `helm install kube-vnet oci://ghcr.io/lhns/charts/kube-vnet --version 0.1.0`.
- Keyless container-image and Helm-chart signing via `cosign` (Sigstore /
  GitHub OIDC). No long-lived keys; verify with the workflow identity.
- SPDX SBOMs for both the image and the chart, attached as `cosign`
  attestations and as plain `.spdx.json` release assets.
- `--version` flag on the operator binary; the binary is stamped at build
  time with version / commit / build-date via `-ldflags`.
- `CHANGELOG.md` (this file).

### Changed

- Baseline ownership moved to the `NamespaceReconciler`. The
  `VirtualNetworkReconciler` no longer touches the baseline. The two
  reconcilers now have clear, narrow ownership: per-vnet membership
  policies vs. per-namespace baseline lifecycle.
- The release workflow now publishes signed artifacts and SBOMs in addition
  to the existing multi-arch container image and `release.yaml`.

### Renamed

- **`operator.excludedNamespaces` → `operator.disabledNamespaces`**, and CLI
  flag **`--excluded-namespaces` → `--disabled-namespaces`**. Mirrors the
  per-namespace `kube-vnet/disabled=true` annotation key. The default is now
  `[]` (the three control-plane namespaces moved to
  `operator.ingressIsolation.namespaceOverrides.none` — see "Behavior
  changes" above). Old forms are accepted for one release with a startup /
  `NOTES.txt` deprecation warning, and will be removed in the next release.
- **`operator.ingressIsolation.forceNone`/`forceNamespace`/`forcePod`** →
  **`operator.ingressIsolation.namespaceOverrides.{none,namespace,pod}`**.
  The new keys nest under `ingressIsolation` and read the same way the
  resolution rule reads ("override to mode `none`", etc.). Old keys are
  accepted for one release with a `NOTES.txt` deprecation warning. CLI flag
  names (`--ingress-isolation-none`, `--ingress-isolation-namespace`,
  `--ingress-isolation-pod`) are unchanged — only the chart-side keys moved.

### Deprecated

- `--default-deny-everywhere` operator flag and
  `operator.defaultDenyEverywhere` Helm value. Aliased to
  `--ingress-isolation=pod` (with a startup deprecation warning) when
  `--ingress-isolation` is at its default. **Will be removed in a future
  release.** Migrate to `--ingress-isolation` and the
  `operator.ingressIsolation.*` Helm values.

### Superseded ADRs

- ADR 0006 — single per-namespace opt-out. Superseded by ADR 0023
  (decoupled `disabled` and `ingress-isolation`).
- ADR 0020 — `--default-deny-everywhere`. Superseded by ADR 0024 (the
  flag family) and ADR 0025 (the rename + ingress-only scope).

## [0.0.0] — initial publication

The first commit set, before tagging. Captured here as a placeholder; the
first tagged release will be `v0.1.0` and roll the Unreleased entries above
into a proper section.

For the substance of what's in the project today (CRD, reconciler, baseline
GC, drift correction, integration + e2e tests, etc.), see the ADRs under
[`docs/adr/`](docs/adr/README.md).
