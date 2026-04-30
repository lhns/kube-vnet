# 0015 — controller-runtime as the operator library

Status: Accepted

## Context

There are several Go libraries and frameworks for building Kubernetes operators:

- `sigs.k8s.io/controller-runtime` — the canonical library maintained under SIG API Machinery. Provides `Manager`, watches, caches, predicates, leader election, the `Reconciler` interface, server-side apply helpers.
- `kubebuilder` — a scaffolding tool that generates a project layout, RBAC markers, and CRD manifests. Uses controller-runtime under the hood; not a separate library.
- `operator-sdk` — Red Hat's wrapper. Also uses controller-runtime; adds optional Helm/Ansible operator types.
- `kcp` / `cluster-api` patterns — domain-specific extensions on top of controller-runtime.

There is no meaningfully better alternative for a reconciliation-loop operator written in Go. Anything that talks to a Kubernetes apiserver from Go ends up using `client-go` either directly or through controller-runtime's facade.

## Decision

Use **`sigs.k8s.io/controller-runtime`** (currently `v0.19.x`). Project layout follows kubebuilder conventions (`api/`, `cmd/`, `internal/controller/`, `config/`) but the kubebuilder CLI is not required to develop the project — `controller-gen` is sufficient for codegen.

No higher-level abstraction is layered on top.

## Consequences

- **Pro**: Same library every Kubernetes operator in the ecosystem uses. Bug fixes, security patches, and new features land upstream regularly.
- **Pro**: Familiar shape for any Kubernetes engineer: `Reconciler.Reconcile`, `mgr.GetClient()`, `For(...).Watches(...).Complete(r)`.
- **Pro**: `controller-gen` produces deepcopy + CRD + RBAC manifests from Go markers; no hand-written YAML.
- **Con**: controller-runtime tracks Kubernetes version compatibility tightly (e.g. `v0.19` ↔ k8s `1.31`). Library upgrades must move in step with the cluster baseline.
- **Note**: `kubebuilder` was *not* used to scaffold the initial project (it's not on the developer's PATH); files were authored directly. Both approaches yield the same layout.
