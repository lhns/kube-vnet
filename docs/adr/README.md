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
