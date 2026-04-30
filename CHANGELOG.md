# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the API surface is `v1alpha1`, breaking changes are possible across any
release. Pinning to an exact version is recommended.

## [Unreleased]

### Added

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

- The release workflow now publishes signed artifacts and SBOMs in addition
  to the existing multi-arch container image and `release.yaml`.

## [0.0.0] — initial publication

The first commit set, before tagging. Captured here as a placeholder; the
first tagged release will be `v0.1.0` and roll the Unreleased entries above
into a proper section.

For the substance of what's in the project today (CRD, reconciler, baseline
GC, drift correction, integration + e2e tests, etc.), see the ADRs under
[`docs/adr/`](docs/adr/README.md).
