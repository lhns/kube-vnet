# 0036 — Helm pre-delete hook removes operator-managed NetworkPolicies on uninstall

Status: Accepted

Date: 2026-06-25

Refines: [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (baseline + membership policy lifecycle), [ADR 0032](0032-chart-crd-removal-two-release-pattern.md) (CRD-keep pattern for upgrade/uninstall safety).

## Context

`helm uninstall kube-vnet` removes the operator Deployment, ServiceAccount, RBAC, and the `ValidatingAdmissionPolicy` objects. It does NOT remove the `NetworkPolicy` objects the operator generated at runtime — the deny-all baseline in each managed namespace and the per-`(vnet, namespace)` membership policies. Those were created by the controllers, not rendered by the chart; Helm has no metadata that ties them to the release.

The result: after uninstall, the cluster is left with frozen enforcement and no controller to keep it in sync.

- Every previously-managed namespace still has its `kube-vnet` baseline (`PodSelector: {}`, `policyTypes: [Ingress]`, no allow rules → deny-all).
- Every member-bearing namespace still has its `kube-vnet.<homeNS>.<vnet>-<hash>` membership policies pinning the old peer lists.
- New pods don't get `kube-vnet.system/net.*` labels stamped (the resolution controller is gone). They're selected by the baseline → deny-all → unreachable.
- Existing pods that should transition (label edits, namespace flips, vnet edits) have no controller to drive the transition. They stay in whatever state they were in at uninstall time.

ADR 0032 documented the same uninstall-cleanliness problem from the *CRD* angle (we keep CRDs across uninstall so reinstall doesn't churn data). The complementary half — the runtime `NetworkPolicy` objects — has been left to manual cleanup: `kubectl delete networkpolicy -A -l kube-vnet/managed-by=kube-vnet`. In practice this is too easy to miss, and the symptom (cluster stuck in deny-all) is far from the cause (a missing cleanup command after an uninstall the user did days ago).

## Decision

Ship a Helm `pre-delete` hook bundle in the chart that automates the manual cleanup. The hook consists of four resources, all tagged with the same hook metadata so they're created together, run together, and cleaned up together:

1. `ServiceAccount` (in the release namespace) — identity for the cleanup pod.
2. `ClusterRole` — `list`, `delete`, `deletecollection` on `networkpolicies.networking.k8s.io` cluster-wide.
3. `ClusterRoleBinding` — binds the cluster role to the service account.
4. `Job` — runs `kubectl delete networkpolicy --all-namespaces --selector kube-vnet/managed-by=kube-vnet --ignore-not-found --wait=true`.

Annotations on every object:

```yaml
helm.sh/hook: pre-delete
helm.sh/hook-weight: "<-10 for the RBAC bundle, 0 for the Job>"
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
```

Hook weight ordering guarantees the RBAC exists before the Job pod starts. The delete policy makes the hook idempotent across repeat installs (Helm wipes prior hook objects before creating new ones) AND auto-cleans on success. If the Job *fails*, the hook objects stay so the user can `kubectl logs` the Job pod.

The selector `kube-vnet/managed-by=kube-vnet` matches both the baseline (`role=baseline`) and the membership policies (`role=membership`). No other operator artifact carries this label, so the selector is precise.

Toggle: `cleanup.enabled` in `values.yaml`, default `true`. Disable for controlled migrations where the operator is being replaced and the existing policy set should persist across the brief outage.

### What this hook does NOT touch

By design:

- **CRDs** (`virtualnetworks`, `virtualnetworkbindings`, `virtualnetworkbaselines`, `clustervirtualnetworkbaselines`). They have `helm.sh/resource-policy: keep` (per ADR 0032) — uninstall preserves them so reinstall lands cleanly.
- **CR instances** — user-authored vnets, bindings, namespace baselines, and the `ClusterVirtualNetworkBaseline` `default` (also `keep`'d by Helm). Survive uninstall by design.
- **System-vnet CRs** (`namespace` per-NS, `cluster` in operator NS). Created at runtime by `SystemVnetReconciler`; not chart-templated. The user can delete them manually if they want a truly clean slate (`kubectl delete vnet -A -l kube-vnet/system=true`).

The hook removes only enforcement (`NetworkPolicy`), not configuration (`VirtualNetwork`/baseline CRs).

## Consequences

- **No more deny-all stuck state** after `helm uninstall`. Effective ingress collapses to Kubernetes default-allow across previously-managed namespaces immediately after the hook runs.
- **Slight uninstall slowdown**: one extra image pull + one Job pod run (~5–10s on a warm cluster). Acceptable for the safety win.
- **One additional ClusterRole transiently exists** during uninstall, with `delete` on `networkpolicies` cluster-wide. Scoped tightly: only the cleanup SA holds it, only for the hook's duration, auto-deleted on success.
- **Failure mode**: if the Job fails (image pull error, apiserver outage, RBAC drift), Helm reports `pre-delete hook failed` and refuses to proceed with the rest of the uninstall. The hook objects stay so the user can `kubectl logs` the Job pod and diagnose. Escape hatch: `helm uninstall --no-hooks` skips the cleanup; manual `kubectl delete networkpolicy -A -l kube-vnet/managed-by=kube-vnet` finishes the job.
- **Existing installations** that were uninstalled before this ADR shipped still have orphan policies. They need a one-time manual cleanup. The hook only fires on future uninstalls.

## Alternatives considered

- **`OwnerReference` from the operator Deployment to every generated policy.** Impossible in stock Kubernetes — owner references can't cross namespaces, and most membership policies live outside the operator's release namespace.
- **Finalizer on each generated policy, processed by the operator on shutdown.** Fragile: races SIGTERM, fails if the operator pod is force-killed, and orphans the finalizer if the operator never comes back. Worse failure mode than today.
- **Document the manual `kubectl delete` step in NOTES.txt.** That's been the state. User reports demonstrate it's too easy to miss; the failure mode (stuck deny-all) is severe.
- **Reuse the operator's existing ServiceAccount.** The operator SA already has `delete networkpolicy` via its main ClusterRole. The hook could use it instead of a dedicated SA. Rejected because it couples the hook's lifecycle to the operator's RBAC — if the operator's RBAC changes shape, the hook silently breaks. A self-contained hook with its own minimal RBAC is more robust.
- **Pre-upgrade hook with the same logic.** Upgrades preserve the operator and its drift correction; the policies are kept in sync across versions. No cleanup needed mid-upgrade. Out of scope.

## References

- [ADR 0030](0030-unified-vnet-membership-with-resolution.md) — baseline + membership policy lifecycle.
- [ADR 0032](0032-chart-crd-removal-two-release-pattern.md) — CRD-keep pattern; this ADR is the runtime-resource counterpart.
- Helm hooks reference: https://helm.sh/docs/topics/charts_hooks/
