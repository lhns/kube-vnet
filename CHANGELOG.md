# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the API surface is `v1alpha1`, breaking changes are possible across any
release. Pinning to an exact version is recommended.

## [Unreleased]

### Fixed

- **Cluster system vnet was rejected as `HomeNamespaceExcluded` on every
  install.** The cluster system vnet's home namespace is the operator
  namespace, which `cmd/main.go` implicitly adds to `disabledNamespaces` as
  a privilege-boundary safety measure. `VirtualNetworkReconciler` then
  rejected any vnet whose home namespace was disabled, so the cluster vnet
  reconciled to `Ready=False, Reason=HomeNamespaceExcluded` and the
  `kube-vnet.system/net.cluster` membership policy never materialized.
  System-labeled vnets (`kube-vnet/system=true`) are now exempt from the
  home-namespace check; the new admission gate below keeps the label
  honest.
- **Helm chart's `ClusterRole` was missing verbs added by ADR 0030.** The
  resolution controller's `pods/patch+update` and the system-vnet
  controller's `virtualnetworks/create+update+patch` were never mirrored
  into `charts/kube-vnet/templates/clusterrole.yaml`, and the new
  `clustervirtualnetworkbindings` group was absent entirely. Helm-installed
  operators failed to stamp `kube-vnet.system/*` labels and never created
  the `namespace` or `cluster` system vnets. A new drift gate
  (`internal/controller/chart_rbac_test.go`) now compares the rendered
  chart ClusterRole against the kubebuilder-generated `config/rbac/role.yaml`
  on every CI run; future divergence fails the chart-manifest job.
- **CRDs now update on `helm upgrade`.** Previously the CRDs lived under
  `charts/kube-vnet/crds/`, which Helm only applies on first install — so
  upgrading a previously-installed chart to a version that introduced a new
  CRD (e.g. `ClusterVirtualNetworkBinding` from ADR 0030) silently skipped
  the new CRD, and the operator looped on `no matches for kind`. The CRDs
  are now templated under `charts/kube-vnet/templates/crd-*.yaml` with
  `helm.sh/resource-policy: keep` so `helm uninstall` doesn't cascade-delete
  user-authored CRs.

  **Migration for users on the broken intermediate version**: the previously
  installed CRDs were created by Helm via the special `crds/` directory,
  which does not stamp Helm ownership metadata. Now that CRDs are
  templated, Helm refuses to adopt them with `invalid ownership metadata;
  ... must be set to "Helm"`. Stamp the labels/annotations once, then
  upgrade — substitute your release name/namespace if different:

  ```bash
  RELEASE=kube-vnet
  NS=kube-vnet-system
  for crd in virtualnetworks.kube-vnet.lhns.de \
             virtualnetworkbindings.kube-vnet.lhns.de \
             clustervirtualnetworkbindings.kube-vnet.lhns.de; do
    kubectl get crd "$crd" >/dev/null 2>&1 || continue
    kubectl label    crd "$crd" app.kubernetes.io/managed-by=Helm --overwrite
    kubectl annotate crd "$crd" meta.helm.sh/release-name="$RELEASE" --overwrite
    kubectl annotate crd "$crd" meta.helm.sh/release-namespace="$NS" --overwrite
  done

  helm upgrade --install "$RELEASE" oci://ghcr.io/lhns/charts/kube-vnet \
    --namespace "$NS"
  kubectl rollout restart deploy -n "$NS" "$RELEASE"
  ```

  Helm will create the missing `ClusterVirtualNetworkBinding` CRD itself
  on the next upgrade now that CRDs are templated.

  **k0s users (helm extension):** the `helm upgrade` step is replaced by
  editing `k0sctl.yaml` to pin the new chart version and `k0sctl apply`.
  The k0s helm reconciler does not re-trigger on `.metadata.annotations`
  changes, so if it has already cached the old error, force a re-reconcile
  by bumping `.spec` on the chart CR:

  ```bash
  kubectl -n kube-system patch chart k0s-addon-chart-kube-vnet \
    --type=merge -p '{"spec":{"timeout":"5m0s"}}'
  ```

  After this fix lands, future upgrades self-heal — no manual step needed.

### Changed

- **VirtualNetwork admission now reserves the names `namespace` and
  `cluster` and the `kube-vnet/system=true` label for the operator.** The
  `system-vnet-protected` ValidatingAdmissionPolicy, previously gating only
  UPDATE/DELETE, now also gates CREATE: a non-operator request to create a
  vnet named `namespace`, named `cluster`, or carrying the system label is
  rejected at admission. The operator's ServiceAccount is exempted via a
  username equality check. Existing user-owned vnets with these shapes
  continue to function (the policy only fires on new CREATE/UPDATE/DELETE
  attempts), but new attempts will be rejected — rename to a non-reserved
  name. There is no automated migration; auto-deletion would risk losing
  user data. Requires Kubernetes >= 1.30 (where ValidatingAdmissionPolicy
  is GA); on older clusters the SystemVnetReconciler's drift-correction
  remains the fallback as before.

### Breaking

- **CLI flags reworked under ADR 0030.** Operator-default vnet memberships
  are declared via the new `--default-memberships=<vnet>=<dir>,...` flag
  (the resolution controller stamps `kube-vnet.system/net.<vnet>` labels
  on every pod accordingly). Pods that should be excluded from the
  always-deny-all baseline are listed via `--elide-baseline-for=<csv>`.
  **The `--ingress-isolation`, `--ingress-isolation-{none,namespace,pod}`
  flags are removed** along with the `kube-vnet/ingress-isolation`
  per-namespace annotation; the cluster-wide-isolation knob's job is
  done by `--default-memberships` now. Helm chart values: replace
  `operator.ingressIsolation.*` with `operator.defaultMemberships` and
  `operator.elideBaselineFor`.
- **Baseline shape changed.** Per-mode baselines (allow-all/same-NS/deny-all)
  are gone. The baseline is always deny-all with `podSelector` excluding
  receivers on the elide-list. ADR 0029's mode=none allow-all and ADR 0023's
  mode-specific baselines are both superseded by ADR 0030.
- **NetworkPolicy podSelectors now key on `kube-vnet.system/net.<vnet>`**
  (operator-stamped by the resolution controller) rather than the user-input
  `kube-vnet/net.<vnet>`. The user-input scheme remains the authoring surface.
- **System VirtualNetworks `namespace` (per-NS) and `cluster` (cluster-wide)**
  exist as managed CRs in every managed namespace and the operator namespace.
  Marked `kube-vnet/system=true`; protected against user mutation by a
  ValidatingAdmissionPolicy.
- **Direction enum pruned.** Join-label values are now exactly `both` /
  `ingress` / `egress` / `none`. The legacy aliases `true` and `false`,
  and the empty-string value, are dropped. The chart's
  `ValidatingAdmissionPolicy` rejects pods carrying legacy values at admission;
  on older clusters the operator's `UnknownDirection` reason on the vnet's
  Degraded condition catches them at reconcile. Migrate `=true`→`=both` and
  `=false`/`=""`→`=none`. Per ADR 0030 (with the ADR 0021 2026-05-05 addendum).
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
