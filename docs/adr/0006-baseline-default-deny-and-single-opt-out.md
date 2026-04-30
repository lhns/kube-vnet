# 0006 — Default-deny baseline and single per-namespace opt-out

Status: Accepted (supersedes the design doc's `kube-vnet/baseline: disabled` annotation)

## Context

Kubernetes' default networking is allow-all: a pod with no `NetworkPolicy` covering it can reach any other pod in the cluster. That makes the VirtualNetwork abstraction *decorative* unless the operator also installs a default-deny baseline in every namespace where vnets are in use. Without the baseline, "pods on different VirtualNetworks cannot communicate" is not actually true.

The design doc proposed two separate knobs:

1. The per-namespace annotation `kube-vnet/baseline: disabled` to opt out of just the baseline.
2. Implicit "this namespace has VirtualNetwork members" inferred from pod labels.

Splitting the baseline opt-out from "is this namespace operator-managed at all?" is incoherent: the baseline *is* what makes the operator meaningful in a namespace. Two knobs invite confusion.

## Decision

A single per-namespace annotation:

```yaml
metadata:
  annotations:
    kube-vnet/disabled: "true"
```

When set, the operator does **nothing** in that namespace:

- No `kube-vnet-default-deny` baseline.
- No membership policies for pods in this namespace.
- Pods here are not eligible joiners for any VirtualNetwork (regardless of `allowedNamespaces`).

When unset (default), in any namespace that has at least one pod joining a VirtualNetwork, the operator ensures `kube-vnet-default-deny` exists. The baseline allows egress to CoreDNS only (UDP+TCP/53 to `kube-system, k8s-app=kube-dns`); everything else is denied unless an additional `NetworkPolicy` permits it.

## Consequences

- **Pro**: One knob, one mental model. "Is this namespace managed by kube-vnet?" → check one annotation.
- **Pro**: Removes the foot-gun where a user could disable the baseline but still get policies, leaving them with no isolation at all.
- **Pro**: User-managed `NetworkPolicy` resources still work — Kubernetes ORs all matching policies, so the user's allow-rules compose with the baseline's denies.
- **Con**: If a user genuinely wanted "policies but no baseline" they can't have it. We believe this combination is wrong by construction; if a real use case appears, a separate ADR can revisit.
