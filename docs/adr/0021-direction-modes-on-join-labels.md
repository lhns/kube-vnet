# 0021 — Direction modes on join labels

Status: Accepted

## Context

Until now the join label value was a presence check: `kube-vnet/net.payments=true` (or any value) meant "this pod is a member." The operator generated a single membership policy per (vnet, namespace) with both `Ingress` and `Egress` rules covering all peers.

Real workloads aren't always symmetric:

- A **logging sidecar** sends data to a collector but should not accept inbound. Egress-only.
- An **auditable read-only API** accepts queries from clients but doesn't initiate calls back. Ingress-only.
- A **regular service** does both. Bidirectional.

Without per-pod direction control, the operator over-allows. The membership policy says "every member can ingress from every other member, every member can egress to every other member" — even when the workload only needs one direction.

## Decision

The label value carries a direction. Recognized values:

| Value | Meaning |
|---|---|
| `both` | Bidirectional. Accept ingress from peers; initiate egress to peers. |
| `ingress` | Accept-only. Accept ingress from peers; do not initiate to them. |
| `egress` | Initiate-only. Send egress to peers; do not accept from them. |
| `none` | Not a member. Same as label absent. |

Compat aliases (preserve every existing manifest):

| Legacy | Maps to |
|---|---|
| `"true"` | `both` |
| `""` (empty) | `both` (presence-only meant member) |
| `"false"` | `none` |

Unknown values (typos like `"bothh"`) are rejected as members AND surfaced on the vnet's `Degraded` condition with reason `UnknownDirection` naming the offending pods. No silent ignore.

### Generated NetworkPolicy shape

The operator now generates **up to three policies per (vnet, namespace)** — one per direction class with at least one member:

- `kube-vnet-<vnet>-<ns>` (no suffix) — bidirectional members. `podSelector: In [true, both]`. `policyTypes: [Ingress, Egress]`. The unsuffixed name preserves the v1alpha1 policy naming for the common case.
- `kube-vnet-<vnet>-<ns>-ingress` — ingress-only members. `podSelector: In [ingress]`. `policyTypes: [Ingress]`. No egress section.
- `kube-vnet-<vnet>-<ns>-egress` — egress-only members. `podSelector: In [egress]`. `policyTypes: [Egress]`. No ingress section.

Direction classes with zero members in a given namespace produce no policy.

### Peer rules narrowed

Peer rules use `In` selectors over the join label value to match the right direction class on the peer side:

- **Ingress allows** (peers that can send to me) match peers in `[true, both, egress]` — those are the peers that can initiate egress.
- **Egress allows** (peers I can send to) match peers in `[true, both, ingress]` — those are the peers that can accept ingress.

### Traffic-flow algebra

X→Y flows iff X has egress capability (`both` or `egress`) AND Y has ingress capability (`both` or `ingress`):

| X mode | Y mode | X→Y | Y→X |
|---|---|---|---|
| both | both | ✓ | ✓ |
| both | ingress | ✓ | ✗ |
| both | egress | ✗ | ✓ |
| ingress | ingress | ✗ | ✗ |
| ingress | egress | ✗ | ✓ |
| egress | egress | ✗ | ✗ |

## Consequences

- **Pro**: Per-pod expressiveness. Workloads model their direction needs explicitly.
- **Pro**: Backward-compatible. Every existing `=true` manifest continues to work. The unsuffixed bidirectional policy keeps its v1alpha1 name; `-ingress` / `-egress` suffixed policies only appear when those direction classes are in use.
- **Pro**: Errors surface. Typos no longer silently make pods non-members.
- **Con**: More policies in the cluster when direction classes are mixed. Up to 3× the policy count per namespace per vnet.
- **Con**: Reviewers need to learn the four enum values. Mitigated by the `kubectl describe vnet` showing direction-class breakdown in `status.members` (planned).
- **Con**: Same vnet seen from different direction modes can confuse a quick scan. Mitigated by the `kube-vnet/role=membership` label still distinguishing membership policies from baselines, plus the policy-name suffixes (`-ingress` / `-egress`) being human-meaningful.

## Cross-references

- ADR 0003 — One label per VirtualNetwork. Direction is encoded in the value of that label, not as a separate label.
- ADR 0009 — Server-side apply. New direction-class policies are applied with the same field manager; drift correction works identically.
- ADR 0022 — Long-form join label in the home namespace. Both ADRs interact: when the long form is in use in the home namespace alongside the bare form, each direction class can produce two policies (one per form) — see ADR 0022.
