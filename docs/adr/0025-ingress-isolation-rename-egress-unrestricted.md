# 0025 — `ingress-isolation` rename + ingress-only scope

Status: Superseded by [ADR 0030](0030-unified-vnet-membership-with-resolution.md) (2026-05-05) and further refined by [ADR 0031](0031-baseline-tier-resolution.md). The `ingress-isolation` annotation and flags are gone; baseline shapes are uniformly deny-all under ADR 0030. The egress-unrestricted decision (this ADR's central claim) is preserved.

## Context

Two related changes that ship together:

1. The rename: `denydefault` (proposed in ADR 0020 as `default-deny-everywhere`) becomes `ingress-isolation`. The new name is a three-valued enum (`none` / `namespace` / `pod`) on a per-namespace annotation, paralleled by an operator-level config.

2. The behavior change: kube-vnet's baseline no longer restricts egress. The baseline carries `policyTypes: [Ingress]` only. Membership policies still grant egress allows to vnet peers, but generic egress (DNS, the apiserver, public internet, other namespaces) is unrestricted by kube-vnet.

## Decision

### Naming: `ingress-isolation`

Rationale:

- **Says what it does.** "Deny default" is a NetworkPolicy implementation detail; "ingress isolation" is the user-facing intent. The same naming applies to the namespace annotation, the CLI flag, the Helm value, and the Go type (with the orthography conventions documented in the plan).
- **Reserves the surface for `egress-isolation`** as a possible future addition. The `ingress`-prefix in every name keeps the symmetry available without committing to it (see "Future work" below).
- **Three values are obvious from the names.** `none` = no isolation, `namespace` = same-namespace ingress allowed, `pod` = strict deny.

### Egress: kube-vnet doesn't restrict it

The baseline becomes:

```yaml
spec:
  podSelector: {}
  policyTypes: [Ingress]   # Ingress only
  ingress: <varies by mode>
  # no egress field
```

Per mode:

- `none` → no baseline at all.
- `namespace` → ingress allow from same-namespace pods only.
- `pod` → no ingress allow rules (deny all).

Membership policies still allow egress between vnet peers (so same-vnet traffic continues to work). The DNS allow rule that membership policies used to include is gone — egress is unrestricted, so DNS resolution works without an explicit allow.

### Why this is the right shape

- The previous "deny everything except DNS + vnet members" was a half-measure. It blocked egress to the internet and other namespaces, but allowed broad egress within vnet membership — typically lots of stuff. It pretended to provide egress isolation while only providing it for the narrow case of "non-vnet, non-DNS destinations."
- Properly scoped egress restriction is per-workload knowledge: `payments-frontend` legitimately needs Stripe, `database` should reach nothing outside the cluster, `audit-logger` only needs its sink. A namespace-level enum can't capture that.
- The user-facing name now matches the actual scope. No surprising "I set ingress-isolation but it also affects my egress."

### Why we don't ship `egress-isolation` as a parallel

Genuinely an open question. The `ingress`-prefix in every name keeps the naming surface free for a future addition, but kube-vnet committing to ship one would be a half-promise.

The honest position:

- A namespace-level mode enum (`none`/`namespace`/`pod`) for egress would have the same problem as today's "deny except vnet members" baseline — too coarse to actually restrict the destinations that matter.
- A real egress story probably wants a separate CRD (e.g. `EgressPolicy`) that names allowed destinations explicitly per workload.
- Today's right answer for users who need egress restriction: write a user-managed `NetworkPolicy` with `policyTypes: [Egress]`. Cluster-level egress firewalls (Calico GlobalNetworkPolicy, NAT-gateway egress allowlists, service mesh egress proxy) handle the cluster-boundary case better than anything kube-vnet could ship.

## Consequences

- **Pro**: Name matches behavior. `ingress-isolation` only does what its name says.
- **Pro**: Simpler model. One enum, one baseline shape per mode, no DNS-allow-handling gymnastics.
- **Pro**: Future-flexible. `egress-isolation` can be added later without breaking compat (or not added at all if the EgressPolicy approach turns out cleaner).
- **Con**: **Behavior change** vs the previous baseline. Today's restricted egress (deny except DNS + vnet members) was a defense-in-depth measure that's now gone. Users who relied on it for egress isolation against compromised pods (exfiltration, lateral probing, misconfiguration containment) need to add a user-managed `NetworkPolicy` or use a cluster-level egress firewall.
- **Con**: The `policy_generator` and the integration tests need updating; the membership policies' DNS allow rule is gone.

### The false-security trap (documented in `docs/security/security.md`)

Configuring `ingress-isolation: pod` does NOT prevent a compromised pod from initiating outbound traffic. The vnet abstraction defends against unauthorized inbound; protecting against outbound exfiltration / lateral probing is a separate concern that needs separate tooling. Documentation makes this loud and explicit.

## Cross-references

- ADR 0006 — single per-namespace opt-out. Superseded by ADR 0023, which decouples `disabled` from baseline control; this ADR (0025) defines what "baseline control" means under the new model.
- ADR 0020 — `--default-deny-everywhere`. Superseded by ADR 0024 for the operator-config rename, and by this ADR for the rename + egress-unrestricted reshape.
- ADR 0019 — baseline durability. The drift-correction defense applies identically to the new baseline shape.
- ADR 0023 — decoupled `disabled` and `ingress-isolation`. Companion to this ADR.
