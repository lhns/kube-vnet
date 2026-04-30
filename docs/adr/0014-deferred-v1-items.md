# 0014 — Deferred v1 items

Status: Accepted (most items resolved; see below)

## Context

The design doc (`docs/kube-vnet-design.md`) lists items required for a "complete" v1 that were not implemented in the initial pass. This ADR tracks them. Most have since landed; the remaining one is recorded explicitly so it isn't forgotten.

## Decision

### Resolved (post-initial-pass)

- **Prometheus custom metrics** — implemented. Six metrics registered with the controller-runtime metrics registry on the `:8080` endpoint:
  - `kube_vnet_reconciliations_total{result}` (counter)
  - `kube_vnet_reconcile_duration_seconds` (histogram)
  - `kube_vnet_networks_total` (gauge)
  - `kube_vnet_managed_policies_total` (gauge)
  - `kube_vnet_members_total{network}` (gauge)
  - `kube_vnet_apply_errors_total{kind}` (counter)

  The first two are updated per-reconcile. `members_total` is updated at the end of a successful reconcile and cleared on vnet deletion. `networks_total` and `managed_policies_total` are updated by a periodic 30-second collector (`MetricsCollector`) that reads cluster-wide state, to avoid biasing toward whichever vnet most recently reconciled.

- **envtest controller suite** — implemented in `internal/controller/integration_test.go` (build tag `integration`). Run via `make integration-test`. See [ADR 0018](0018-test-strategy-envtest-and-kind-calico.md).

- **kind+Calico end-to-end suite** — implemented in `test/e2e/` (build tag `e2e`). Bootstrapped by `test/e2e/up.sh` or `.github/workflows/e2e.yaml`. See [ADR 0018](0018-test-strategy-envtest-and-kind-calico.md).

### Still deferred

- **Label cardinality stress test** — design § Open Questions, Q5. Verify that pods joining N vnets (large N, e.g. 50+) don't degrade the watch predicate or selector path in measurable ways. Best added as one more case in the e2e suite (or a separate benchmark) once the e2e lane is proven stable in CI. Not blocking v1 for typical workloads (1–5 vnets per pod).

- **AdminNetworkPolicy adoption for the deny baseline** — see [ADR 0019](0019-baseline-durability.md). The current drift-correction defense is sufficient for the common threat (accidental deletion); ANP becomes the proper answer for hard isolation guarantees once CNI support is universal and the API lands at v1.

## Consequences

- **Pro**: The original ADR-0014 list is now mostly closed; what remains is small and well-scoped.
- **Pro**: Operators have domain-specific Prometheus signal (`kube_vnet_*`) for alerting beyond the controller-runtime defaults.
- **Pro**: PRs gain three test rungs of feedback: unit (sub-second), integration (~10s), e2e (~5–8 min).
- **Pro**: The cardinality stress test is the only remaining item; it's a benchmark, not a correctness gap.
