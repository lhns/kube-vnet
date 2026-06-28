# Architecture Decision Records

Each ADR captures a single decision: the context, what was decided, and the consequences. ADRs are immutable once accepted — if a decision is reversed, a new ADR supersedes the old one.

For long-form background on the project, see [`../kube-vnet-design.md`](../kube-vnet-design.md). The design doc explains the *what* and *why* at length; ADRs are the short, decision-scoped record that lives alongside the code.

## Index

1. [0001 — VirtualNetwork as a named-network abstraction](0001-virtualnetwork-as-named-network-abstraction.md)
2. [0002 — Emit standard NetworkPolicy only](0002-emit-standard-networkpolicy-only.md)
3. [0003 — One label per VirtualNetwork](0003-one-label-per-virtualnetwork.md)
4. [0004 — Bare vs namespace-prefixed join label](0004-bare-vs-namespace-prefixed-join-label.md)
5. [0005 — Namespaced CRD with allowedNamespaces](0005-namespaced-crd-with-allowed-namespaces.md)
6. [0006 — Default-deny baseline and single per-namespace opt-out](0006-baseline-default-deny-and-single-opt-out.md)
7. [0007 — Operator-level excluded namespaces](0007-operator-level-excluded-namespaces.md)
8. [0008 — Pure-function policy generator](0008-pure-function-policy-generator.md)
9. [0009 — Server-side apply with field manager](0009-server-side-apply-with-field-manager.md)
10. [0010 — Cross-namespace cleanup via the network label](0010-cross-namespace-cleanup-via-network-label.md)
11. [0011 — Policy naming and truncation](0011-policy-naming-and-truncation.md)
12. [0012 — Status conditions: Ready and Degraded](0012-status-conditions-ready-and-degraded.md)
13. [0013 — Pod watch with handler.Funcs for removals](0013-pod-watch-with-handler-funcs-for-removals.md)
14. [0014 — Deferred v1 items](0014-deferred-v1-items.md)
15. [0015 — controller-runtime as the operator library](0015-controller-runtime-as-the-operator-library.md)
16. [0016 — Emit events on condition transitions](0016-emit-events-on-condition-transitions.md)
17. [0017 — Name validation via CEL and runtime check](0017-name-validation-via-cel-and-runtime-check.md)
18. [0018 — Test strategy: unit + envtest + kind+Calico](0018-test-strategy-envtest-and-kind-calico.md)
19. [0019 — Baseline durability via drift correction; AdminNetworkPolicy deferred](0019-baseline-durability.md)
20. [0020 — `--default-deny-everywhere` flag for cluster-wide default-deny](0020-default-deny-unmanaged-namespaces.md)
21. [0021 — Direction modes on join labels (`both` / `ingress` / `egress` / `none`)](0021-direction-modes-on-join-labels.md)
22. [0022 — Long-form join label accepted in the home namespace](0022-long-form-join-label-in-home-namespace.md)
23. [0023 — Decoupled `disabled` and `ingress-isolation` annotations](0023-decoupled-disabled-and-ingress-isolation.md)
24. [0024 — Operator ingress-isolation default + per-mode override lists](0024-ingress-isolation-mode-and-overrides.md)
25. [0025 — `ingress-isolation` rename + egress unrestricted](0025-ingress-isolation-rename-egress-unrestricted.md)
26. [0026 — `VirtualNetworkBinding` CRD as the no-label alternative](0026-virtualnetworkbinding-crd.md)
27. [0027 — Pod-scoped events for join-label diagnostics](0027-pod-scoped-join-label-events.md)
28. [0028 — Runtime policy-enforcement verification (Proposed / draft)](0028-runtime-policy-verification.md)
29. [0029 — Allow-all baseline in mode=none; system namespaces disabled by default](0029-allow-all-baseline-and-system-ns-disabled.md) — *superseded by 0030*
30. [0030 — Unified vnet-membership model with resolution layer](0030-unified-vnet-membership-with-resolution.md) — *resolution-lattice section partially superseded by 0031*
31. [0031 — Baseline-tier resolution: replace bindings/CVNB with explicit defaults vs bindings](0031-baseline-tier-resolution.md)
32. [0032 — Removing a chart-shipped CRD requires a two-release deprecation dance](0032-chart-crd-removal-two-release-pattern.md)
33. [0033 — Canonical fully-qualified system labels; per-binding policies removed](0033-canonical-fq-system-labels.md)
34. [0034 — Mutating admission webhook for synchronous pod-tier resolution (Proposed)](0034-admission-webhook-for-pod-resolution.md)
35. [0035 — Removal of `--elide-baseline-for`: baseline elide had no observable effect](0035-removal-of-elide-baseline-for.md)
36. [0036 — Helm pre-delete hook removes operator-managed NetworkPolicies on uninstall](0036-helm-pre-delete-hook-cleanup.md)
37. [0037 — `kube-vnet.system/` prefix convention for operator-owned keys](0037-system-prefix-convention-for-operator-owned-keys.md)
38. [0038 — Auto-allow externally-exposed Services](0038-auto-allow-externally-exposed-services.md)
39. [0039 — Uniform kind-prefixed naming for operator-emitted NetworkPolicies](0039-uniform-kind-prefixed-policy-naming.md)
40. [0040 — Auto-allow for hostPort pods via per-port stamping](0040-auto-allow-hostport-pods.md)
41. [0041 — Auto-allow Services reached by the apiserver](0041-auto-allow-apiserver-reachable-services.md)

## Format

Each ADR follows MADR-lite:

```
# NNNN — Short title
Status: Accepted | Superseded by NNNN | Deprecated

## Context
Why we needed to decide.

## Decision
What we decided.

## Consequences
What follows from the decision — both the wins and the costs.
```
