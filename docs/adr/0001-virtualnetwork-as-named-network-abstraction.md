# 0001 — VirtualNetwork as a named-network abstraction

Status: Accepted

## Context

Kubernetes' built-in `NetworkPolicy` resource sits at the wrong level of abstraction for how operators and developers naturally reason about service connectivity. Its model is *exception-based* (everything connects unless denied) and *selector-based* (connectivity is described in terms of pod label selectors). The mental model most teams actually use is *membership-based* (services join named networks) and *allowlist-by-construction* (only same-network pods communicate). Docker Swarm's named-network primitive is the canonical example of the mental model that fits.

Translating the membership mental model into raw `NetworkPolicy` resources by hand is repetitive, error-prone (especially the default-deny baseline that is non-decorative), and produces selectors that are hard to review.

## Decision

Introduce a `VirtualNetwork` custom resource. Pods declare membership via labels; the operator translates membership into the underlying `NetworkPolicy` set. Same-VirtualNetwork pods can talk to each other; pods on different (or no) VirtualNetworks are isolated by an automatic default-deny baseline (see ADR 0006).

The "virtual" qualifier is deliberate: a `VirtualNetwork` is a logical grouping, not an actual network plane. Pods continue to traverse the cluster's CNI as normal; the operator merely shapes the `NetworkPolicy` set so connectivity follows membership.

## Consequences

- **Pro**: Users express intent in the membership model that matches how they think; review is simpler ("is service X on the payments network?") than reviewing pod selectors.
- **Pro**: The operator can be removed cleanly — generated policies are plain `NetworkPolicy` and continue to work without the operator.
- **Pro**: A clean foundation for future extensions (cross-cluster peers, identity-aware policy).
- **Con**: A new abstraction layer to learn. Mitigated by familiarity with Docker Swarm's model.
- **Con**: One more controller to operate.
