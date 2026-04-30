# 0003 — One label per VirtualNetwork

Status: Accepted

## Context

Pods need to declare membership in zero or more VirtualNetworks. Two natural encodings:

A. **One label per network**: `kube-vnet/net.payments=true`, `kube-vnet/net.monitoring=true`, …
B. **One label with a delimited list**: `kube-vnet/networks=payments,monitoring,…`.

## Decision

Use one label per network (Option A). Bare form `kube-vnet/net.<name>` for same-namespace references; namespace-prefixed form `kube-vnet/net.<homeNS>.<name>` for cross-namespace references (see ADR 0004).

The operator only checks for *key presence*, not value content. Convention is `"true"` but any value works.

## Consequences

- **Pro**: Generated `NetworkPolicy` selectors are trivial — `matchExpressions` with `operator: Exists` on the relevant key. No enumeration of value combinations, no canonicalization concerns.
- **Pro**: Label values are capped at 63 characters; a comma-separated list of network names blows past that quickly. One label per network has no aggregate ceiling.
- **Pro**: Matches Kubernetes label conventions ("this resource is in category X" is one label per category, not a delimited string in one value).
- **Con**: A pod that joins many networks accumulates many labels. For typical deployments this is trivial; ADR 0014 tracks the stress-test follow-up.
