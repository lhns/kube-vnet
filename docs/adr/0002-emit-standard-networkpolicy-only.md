# 0002 — Emit standard NetworkPolicy only

Status: Accepted

## Context

Several CNIs offer richer policy types than `networking.k8s.io/v1` `NetworkPolicy` — Cilium has `CiliumNetworkPolicy` (L7, DNS-based egress), Calico has `GlobalNetworkPolicy` (tier ordering), and so on. Generating those would unlock features the standard type doesn't support, but at the cost of CNI-specific output and lock-in.

## Decision

v1 generates only `networking.k8s.io/v1` `NetworkPolicy`. No CNI-specific resources.

## Consequences

- **Pro**: Works on any CNI that enforces standard `NetworkPolicy` (Calico, Cilium, Antrea, kube-router, etc.).
- **Pro**: The generated policies survive the operator's removal: they're plain Kubernetes resources.
- **Pro**: Smaller v1 surface; less to test and maintain.
- **Con**: L7 / DNS-based egress / mTLS-identity policy is out of reach. Users that need those reach for service mesh or write CNI-specific policies alongside.
- **Future**: A pluggable backend system (per ADR 0014) can add Cilium/Calico-specific output without breaking the standard path.
