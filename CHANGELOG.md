# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the API surface is `v1alpha1`, breaking changes are possible across any
release. Pinning to an exact version is recommended.

## [Unreleased]

### Breaking

- **Mode=none now materializes an allow-all baseline.** Previously, a managed
  namespace with `ingress-isolation=none` had no `kube-vnet`-owned baseline
  policy. As of this release, mode=none generates a baseline whose ingress
  rule is empty (the K8s idiom for "allow all sources, all ports"). Traffic
  outcome is identical for non-member pods. Vnet member pods in mode=none
  are no longer over-restricted: their effective ingress is now correctly
  "allow-all ∪ vnet peers = allow-all," consistent with the outer-boundary
  model used by the other modes. **Visible effect**: `kubectl get netpol -A`
  in a mode=none namespace now lists one additional `kube-vnet` policy. See
  [ADR 0029](docs/adr/0029-allow-all-baseline-and-system-ns-disabled.md).
- **System namespaces are `disabled` by default again.** `kube-system`,
  `kube-public`, and `kube-node-lease` move from
  `operator.ingressIsolation.namespaceOverrides.none` (chart) /
  `--ingress-isolation-none` (CLI) into `operator.disabledNamespaces` /
  `--disabled-namespaces`. Net effect on most clusters: zero — those
  namespaces remain free of kube-vnet objects. Users who *relied* on the
  previous default to enroll a system-namespace pod in a vnet must remove
  the relevant namespace from `disabledNamespaces`. ADR 0029 supersedes the
  default-placement choice in ADR 0023; the decoupling principle in 0023
  is unchanged.
- **NetworkPolicy names changed.** The previous `kube-vnet-<vnet>-<ns>[-prefixed]`
  / `kube-vnet-<vnet>-b-<binding>` / `kube-vnet-default-deny` scheme allowed
  collisions (e.g. baseline vs membership where vnet=`default` and ns=`deny`,
  or vnet=`foo`/ns=`bar-baz` vs vnet=`foo-bar`/ns=`baz`). New scheme uses a
  dot-separated structural prefix and an 8-hex identity hash:
  - Baseline: `kube-vnet` (per-namespace singleton)
  - Membership bare (home NS): `kube-vnet.<vnet>-<8hex>`
  - Membership prefixed: `kube-vnet.<homeNS>.<vnet>-<8hex>` (mirrors the
    `kube-vnet/net.<homeNS>.<vnet>` label key)
  - Per-binding: `kube-vnet.<homeNS>.<vnet>.b.<binding>-<8hex>`

  The dot prefix avoids visual confusion with dash-bearing namespace names
  (e.g. `netpol-demo`); the 8-hex suffix is an *identity* hash (SHA-256 of
  class+homeNS+vnet[+binding] joined by `\x00`, first 4 bytes hex'd) — stable
  across membership churn.

  Migration is automatic: the existing `deleteStale()` pass GCs old-named
  membership policies on the first reconcile, and the namespace reconciler
  now sweeps stale baseline policies (any policy labelled
  `kube-vnet/role=baseline` whose name differs from the current
  `BaselinePolicyName`). NetworkPolicy is additive, so traffic posture is
  unchanged during the transition. **Action required**: any tooling/scripts
  hard-coded to old policy names must update.

- **Empty join-label value (`kube-vnet/net.X: ""`) now parses as `none`, not
  `both`.** The legacy "presence-only meant member" rule mapped the empty
  string to `both`; that no longer holds. A pod with `kube-vnet/net.X: ""` is
  *not* a member. Update existing manifests to use an explicit `=both` (or the
  legacy `=true` alias) if you intended membership. The legacy `"true"` /
  `"false"` aliases are unchanged. See the
  [ADR 0021 empty-string addendum](docs/adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-04--empty-string-value-reinterpreted-as-none)
  and [ADR 0027](docs/adr/0027-pod-scoped-join-label-events.md).
- **`operator.ingressIsolation.mode` (Helm) and `--ingress-isolation` (CLI) are
  now required.** The chart no longer ships a default for `mode`; `helm install`
  and `helm upgrade` fail fast via `required` if it's empty. The operator
  binary exits non-zero at startup if `--ingress-isolation` is not set
  explicitly. Existing chart users: add `mode: <none|namespace|pod>` to your
  values (or pass `--set operator.ingressIsolation.mode=...`) before
  upgrading. Operator-binary users: add `--ingress-isolation=...` to your
  manager args.

### Behavior changes (upgrade-impacting)

- **`kube-system`, `kube-public`, and `kube-node-lease` are no longer in
  `disabledNamespaces` by default.** They are
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

- **`ValidatingAdmissionPolicy` for join-label direction values
  (Kubernetes ≥ 1.30).** The chart ships a VAP + binding that rejects Pod
  create/update when any `kube-vnet/net.*` label has a value not in
  `[both, ingress, egress, none, true, false, ""]`. Typos like
  `kube-vnet/net.X=bothh` fail at `kubectl apply` instead of being caught
  later by the operator. Older clusters skip the VAP and continue to rely on
  the operator's runtime `Degraded`/`UnknownDirection` reason for the same
  fault. See [ADR 0027](docs/adr/0027-pod-scoped-join-label-events.md).
- **Pod-scoped Warning events for stateful join-label diagnostics.** A new
  `JoinLabelDiagnosticReconciler` watches Pods carrying `kube-vnet/net.*`
  labels and emits Warning events on the Pod itself —
  `BareJoinLabelVnetNotFound` (bare-form label with no vnet of that name in
  the pod's own namespace), `PrefixedJoinLabelVnetNotFound` (prefixed-form
  label naming a vnet that doesn't exist), and `JoinLabelNamespaceNotAllowed`
  (vnet exists but its `spec.allowedNamespaces` excludes the pod's
  namespace). Visible via `kubectl describe pod`. Pods in disabled or
  excluded namespaces are skipped by design. The vnet-status reasons
  (`InvalidJoiners`, etc.) are unchanged — they serve the vnet-owner
  audience; the new pod events serve the pod-owner audience. See
  [ADR 0027](docs/adr/0027-pod-scoped-join-label-events.md).
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

- **Build: dropped QEMU emulation for arm64.** Added
  `--platform=$BUILDPLATFORM` to the Dockerfile builder stage so the Go
  toolchain runs natively on the runner's amd64 even when buildx targets
  arm64. The `RUN go build` already cross-compiled via GOOS/GOARCH; the
  toolchain itself was just being emulated for no good reason. Multi-arch
  releases build noticeably faster.
- **CI: per-commit dev builds.** `release.yaml` now also runs on every
  branch push (and `workflow_dispatch`), producing signed
  `0.0.0-dev.<short-sha>` images and chart tags so users can `helm
  install --version 0.0.0-dev.abc1234` to test a specific commit
  without cutting a release tag. Dev builds are single-arch (amd64) and
  use the GitHub Actions buildx cache; typical wall time is 2–4 min vs
  ~20 min for a full multi-arch release. Image gets additional
  `:sha-<short>` and `:<branch>` tags; chart only the immutable SemVer
  pre-release version. GitHub Release asset upload is gated to
  release-mode (tag pushes only); dev pushes don't touch any Release.
- **Vnet `Degraded`/`InvalidJoiners` message format now includes the per-pod
  reason.** Each per-pod entry in the Degraded message is
  `<ns>/<pod>:<reason>` (was just `<ns>/<pod>`), so users can see which pod
  failed for which reason at a glance via `kubectl describe vnet`. The cap at
  3 entries still applies; the condition's `Reason` field stays
  `InvalidJoiners`.
- Baseline ownership moved to the `NamespaceReconciler`. The
  `VirtualNetworkReconciler` no longer touches the baseline. The two
  reconcilers now have clear, narrow ownership: per-vnet membership
  policies vs. per-namespace baseline lifecycle.
- **Bidi + ingress self-policies merged into one per (namespace,
  key-form).** Since membership policies became ingress-only (ADR 0025),
  the previously-separate bidi (`kube-vnet-<vnet>-<ns>`) and ingress-only
  (`kube-vnet-<vnet>-<ns>-ingress`) self-policies were spec-identical
  except for `podSelector` In-values. They're now a single policy whose
  selector matches `[true, both, ingress]`. The `-ingress` policy-name
  suffix is gone. Existing `-ingress`-suffixed policies are GC'd by
  `deleteStale` on the next reconcile. `egress`-only members continue to
  produce no self-policy. See [ADR 0021 Addendum](docs/adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-04--bidi--ingress-self-policies-merged).
- **Docs canonicalize the join-label value as `"both"`.** Examples
  across `README.md`, `docs/recipes.md`, `docs/kube-vnet-design.md`, and
  `config/samples/01-05` now use `kube-vnet/net.<vnet>: "both"`. The
  legacy `"true"` (and `"false"`) still parse as aliases for `both` and
  `none` respectively — no manifest changes required for existing users.
- The release workflow now publishes signed artifacts and SBOMs in addition
  to the existing multi-arch container image and `release.yaml`.

### Renamed

- **`operator.excludedNamespaces` → `operator.disabledNamespaces`**, and CLI
  flag **`--excluded-namespaces` → `--disabled-namespaces`**. Mirrors the
  per-namespace `kube-vnet/disabled=true` annotation key. The default is now
  `[]` (the three control-plane namespaces moved to
  `operator.ingressIsolation.namespaceOverrides.none` — see "Behavior
  changes" above).
- **`operator.ingressIsolation.forceNone`/`forceNamespace`/`forcePod`** →
  **`operator.ingressIsolation.namespaceOverrides.{none,namespace,pod}`**.
  The new keys nest under `ingressIsolation` and read the same way the
  resolution rule reads ("override to mode `none`", etc.). CLI flag names
  (`--ingress-isolation-none`, `--ingress-isolation-namespace`,
  `--ingress-isolation-pod`) are unchanged — only the chart-side keys moved.

### Removed

The deprecated aliases that were briefly accepted for the renames above are
gone. There is no compatibility shim — installs still using these names must
migrate before upgrading.

- CLI flag `--excluded-namespaces` (use `--disabled-namespaces`).
- CLI flag `--default-deny-everywhere` (use `--ingress-isolation=pod`).
- Helm value `operator.excludedNamespaces` (use `operator.disabledNamespaces`).
- Helm value `operator.defaultDenyEverywhere` (use `operator.ingressIsolation.mode=pod`).
- Helm value `operator.ingressIsolation.forceNone` (use `operator.ingressIsolation.namespaceOverrides.none`).
- Helm value `operator.ingressIsolation.forceNamespace` (use `operator.ingressIsolation.namespaceOverrides.namespace`).
- Helm value `operator.ingressIsolation.forcePod` (use `operator.ingressIsolation.namespaceOverrides.pod`).

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
