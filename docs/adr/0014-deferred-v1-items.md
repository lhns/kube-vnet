# 0014 — Deferred v1 items

Status: Accepted (deferred)

## Context

The design doc (`docs/kube-vnet-design.md`) lists items required for a "complete" v1 that are **not** yet implemented. This ADR records what's owed so future maintainers see the gap is known, not forgotten.

## Decision

The following items are intentionally deferred. Each links back to the relevant design-doc section so the original requirement is auditable.

### Prometheus custom metrics — design § Status / Metrics

Not yet emitted:

- `kube_vnet_reconciliations_total{result="success|error"}` (counter)
- `kube_vnet_reconcile_duration_seconds` (histogram)
- `kube_vnet_networks_total` (gauge — note: the `extent` label dimension from the original design no longer applies, since `extent` was removed per ADR 0005)
- `kube_vnet_managed_policies_total` (gauge)
- `kube_vnet_members_total{network="ns/name"}` (gauge)

The controller-runtime default metrics endpoint (`workqueue_*`, `controller_runtime_*`) is exposed on `:8080`; that covers reconcile latency and queue depth. The custom counters above are domain-specific and remain to be added.

### envtest controller suite — design § Testing / Integration tests

Not yet written. Should cover, at minimum:

- Create a VirtualNetwork → assert NetworkPolicies appear with the expected spec.
- Update `allowedNamespaces` → assert policies are regenerated correctly.
- Delete a VirtualNetwork → assert all owned policies are removed (including in foreign namespaces).
- Cross-namespace join to a non-permitted namespace → assert the join is rejected and `Degraded=True, reason=InvalidJoiners` surfaces.

### kind+Calico end-to-end suite — design § Testing / End-to-end tests

Not yet written. Should exercise actual traffic flow on a kind cluster running a NetworkPolicy-enforcing CNI:

- Two pods on the same VirtualNetwork → curl succeeds.
- Two pods on different (or no) VirtualNetwork → curl times out.
- Cluster-wide vnet (`allowedNamespaces.all=true`) across two namespaces → enforcement works in both directions.

### Label cardinality stress test — design § Open Questions, Q5

Verify that pods joining N vnets (large N) don't degrade the predicate or selector path in measurable ways. Should be exercised in the e2e suite once it exists.

## Consequences

- **Pro**: A single page documenting the gap to v1-complete. Anyone picking up the project knows where to start.
- **Pro**: The implemented core (CRD, generator, reconciler, baseline, drift correction, cross-namespace cleanup, conditions, events) is functionally complete and tested at the unit level. The deferred items are observability and test depth, not correctness.
- **Con**: The operator ships without custom metrics, which limits Prometheus-based alerting on operator-specific signals. Workaround: monitor the controller-runtime defaults plus the Kubernetes Events emitted on Degraded/ApplyFailed (per ADR 0016).
