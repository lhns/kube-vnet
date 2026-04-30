# 0004 — Bare vs namespace-prefixed join label

Status: Accepted

## Context

A VirtualNetwork lives in a "home" namespace but may permit pods from other namespaces to join (per ADR 0005's `allowedNamespaces`). The pod's join label needs to encode *which* VirtualNetwork is being joined, including its home namespace if it's not the pod's own.

## Decision

Two label forms:

- **Bare**: `kube-vnet/net.<vnet-name>` — for pods in the VirtualNetwork's home namespace.
- **Namespace-prefixed**: `kube-vnet/net.<vnet-namespace>.<vnet-name>` — for pods in any other namespace.

The dot separator distinguishes the two forms. A single dot after `net.` means "in this pod's namespace"; two dots means "namespace-prefixed reference."

VirtualNetwork names cannot contain dots (enforced by ADR 0017's name validation), so this encoding is unambiguous.

## Consequences

- **Pro**: A single `Exists` selector key in the generated policy works per (VirtualNetwork, namespace) pair — no need to enumerate cross-namespace references in a more complex selector.
- **Pro**: Label keys make the cross-namespace intent explicit at the pod template level — visible in code review.
- **Con**: Two forms to learn. Documented in `01_same_namespace.yaml` (bare) and `02_…`/`03_…`/`04_…` (prefixed).
- **Constraint**: VirtualNetwork names must be DNS-1123 *labels* (no dots). ADR 0017 records the validation strategy.
