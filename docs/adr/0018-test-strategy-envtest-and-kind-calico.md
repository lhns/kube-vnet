# 0018 — Test strategy: unit + envtest + kind+Calico

Status: Accepted

## Context

The operator needs three different kinds of test signal:

1. Are the pure functions correct? (selector keys, peer enumeration, name truncation, namespace matching)
2. Does the reconciler interact with the API server correctly? (CRD validation, status conditions, drift correction, cross-namespace cleanup, watches reacting to label changes)
3. **Does isolation actually work?** (when two pods are on different VirtualNetworks, can wget really not reach the other)

A single test rung can't deliver all three. Unit tests are blind to API behavior. envtest exercises the API but has no CNI, so it can't observe traffic. Only a real cluster with a NetworkPolicy-enforcing CNI can answer "does the policy actually block packets".

## Decision

Three rungs, each with its own scope and CI lane:

### Rung 1 — Unit tests (`go test ./...`)

Pure-function tests against the policy generator, baseline, namespace-selector logic, name regex, metric registration. Live alongside the code in `internal/controller/*_test.go`. Stdlib `testing.T`.

### Rung 2 — Integration tests (envtest, build tag `integration`)

`internal/controller/integration_test.go` and `suite_integration_test.go` — start a real `kube-apiserver` + `etcd` via `sigs.k8s.io/controller-runtime/pkg/envtest`, install the CRD, start the manager + reconciler, and exercise:

- vnet create → policy appears with the expected selector
- baseline created on first member
- two-namespace allowedNamespaces with foreign joiner
- invalid joiner (foreign namespace not permitted) → Degraded condition
- vnet delete → all owned policies removed
- drift correction (clobber the spec, see it reverted)
- per-namespace `kube-vnet/disabled=true` opts out
- CEL rule rejects names with dots at admission

Build tag `integration` keeps these out of the default `go test ./...` run because they require envtest binaries (downloaded via `setup-envtest`). Run via `make integration-test`. Also run in CI as a separate job.

**No CNI here** — these tests verify what the operator *does*, not what the network *enforces*.

### Rung 3 — End-to-end tests (kind + Calico, build tag `e2e`)

`test/e2e/e2e_test.go` — run against a real `kind` cluster with Calico installed (so NetworkPolicy is actually enforced) and the operator already deployed. Tests use `kubectl exec ... wget` to assert traffic actually flows or doesn't:

- Same vnet → connectivity works
- Different vnets → blocked
- No vnet (in a namespace with a vnet) → blocked by the baseline default-deny
- `allowedNamespaces.all=true` → cross-namespace traffic flows

Build tag `e2e`. Bootstrap is `hack/e2e-up.sh` locally or the `.github/workflows/e2e.yaml` in CI.

## Why these specific choices

### Why stdlib `testing.T` (not ginkgo)

Kubebuilder's default scaffolding uses ginkgo. We don't, for two reasons: the existing tests use stdlib (consistency matters), and the BDD shape adds learning cost without adding signal for our test sizes. `envtest.Environment` works equally well with `TestMain`.

### Why Calico (not Cilium) for the e2e CNI

- Calico is the most widely-used `NetworkPolicy` enforcer; what works on Calico works on most clusters.
- We don't need any feature beyond standard `NetworkPolicy` (per ADR 0002), so Cilium's L7/eBPF advantages don't apply to v1.
- Calico bootstrap on kind is well-documented and stable in CI.

### Why a separate `e2e.yaml` workflow

- Faster lanes (unit, integration, docker) don't have to wait for the slow lane (kind + Calico is ~5 minutes).
- The e2e workflow can be rerun independently when only flake-prone parts need attention.
- Failures in e2e don't gate every PR's "fast green" indicator.

### Why `kind` (not k3s, microk8s, minikube, or a hosted cluster)

`kind` runs in any GitHub Actions Linux runner, has no out-of-band setup, supports `disableDefaultCNI` so Calico can install cleanly, and is the canonical choice for Kubernetes-in-CI. Hosted clusters (EKS/GKE) would be more realistic but slower and gated on cloud credentials.

## Consequences

- **Pro**: Three rungs cover three different failure modes; gaps that one would miss are caught by another.
- **Pro**: The fast feedback loop (`go test ./...`) stays under a second, even as the integration and e2e suites grow.
- **Pro**: `make integration-test` is runnable on developer machines that have Go but no Docker.
- **Con**: Three test layers means three places to keep healthy. Worth it given the operator's blast radius (a misbehaving NetworkPolicy generator can break cluster connectivity).
- **Con**: e2e timing windows (canReach 30s, cannotReach 15s) are heuristic. NetworkPolicy enforcement on kind+Calico is reliable but not instant; if these prove flaky, lengthen the windows or replace `wget` polling with Calico-specific eventual-consistency probes.
