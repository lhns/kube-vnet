# kube-vnet documentation

## New to kube-vnet? Start here

1. [**Concepts**](getting-started/concepts.md) — the mental model: named networks, join labels, directions, the deny-all baseline, cross-namespace rules. ~15 minutes; makes everything else obvious.
2. [**Install**](getting-started/install.md) — Helm (recommended), `kubectl apply`, air-gapped, signature verification.
3. [**Your first VirtualNetwork**](getting-started/first-vnet.md) — the hands-on walkthrough: pick an isolation level, install, create a network, join pods, and *prove* the isolation with a live probe. Covers the CRD inventory, the three membership mechanisms, directions, and what the operator handles automatically.

Then head to [recipes](guides/recipes.md) for real-world patterns.

## The tree

| Directory | What lives there |
|---|---|
| [`getting-started/`](#getting-started) | The linear path above: concepts → install → first network. |
| [`guides/`](#guides) | Task- and topic-oriented: recipes, auto-allow, operations, security, troubleshooting. |
| [`reference/`](#reference) | Look-up tables, not narrative: CRD fields, flags/values, labels, metrics, glossary. |
| [`internals/`](#internals) | For contributors: architecture, source map, dev workflow, the historical design doc. |
| [`adr/`](adr/README.md) | Architecture Decision Records — the **source of truth** wherever prose docs disagree. |
| [`faq.md`](faq.md) | Cross-cutting Q&A: production-readiness, CNI compatibility, conntrack behavior, comparisons. |

### getting-started/

- [`concepts.md`](getting-started/concepts.md) — the model in depth: VirtualNetworks, direction modes (all eight values), `allowedNamespaces` join-eligibility, the baseline tiers, `VirtualNetworkBinding`, how it all maps onto stock NetworkPolicy.
- [`install.md`](getting-started/install.md) — three install paths, CNI prerequisites, upgrading, uninstalling, cosign/SBOM verification, air-gapped installs.
- [`first-vnet.md`](getting-started/first-vnet.md) — the copy-pasteable tutorial with a working probe.

### guides/

- [`recipes.md`](guides/recipes.md) — worked examples: three-tier app, observability network, bridge pods, direction patterns, enrolling third-party pods, migrating an existing namespace, coexisting with user-managed NetworkPolicy, egress allowlists.
- [`auto-allow.md`](guides/auto-allow.md) — the traffic the operator admits without being asked: externally-exposed Services, hostPort pods, and Services the apiserver dials (webhooks, metrics-server). Triggers, opt-outs, the `ext.*` policy naming.
- [`operations.md`](guides/operations.md) — running it in production: topology, HA, leader election, sizing, monitoring, the operational playbooks.
- [`security.md`](guides/security.md) — threat model (including what kube-vnet does *not* defend), RBAC inventory, supply chain, hardening.
- [`troubleshooting.md`](guides/troubleshooting.md) — symptom → diagnosis → fix, from "my pod isn't a member" to admission-webhook timeouts.
- [`cni-pitfalls.md`](guides/cni-pitfalls.md) — CNI-layer enforcement failures (kube-router, k0s, Calico, Cilium) with per-node verification commands and a manual isolation probe.

### reference/

- [`api.md`](reference/api.md) — all four CRDs, every field, every status-condition reason.
- [`configuration.md`](reference/configuration.md) — every operator flag, Helm value, and env var, with defaults rationale.
- [`labels-and-annotations.md`](reference/labels-and-annotations.md) — the complete label/annotation contract: what you write, what the operator writes, what tooling can rely on.
- [`metrics-and-events.md`](reference/metrics-and-events.md) — every metric and Kubernetes Event, with sample alert rules.
- [`glossary.md`](reference/glossary.md) — defined terms.

### internals/

- [`architecture.md`](internals/architecture.md) — the runtime structure: reconcilers, watches, the pure-function generator, drift correction.
- [`code-structure.md`](internals/code-structure.md) — the current file-by-file source map and the resolution data flow (the single source; other pages point here).
- [`development.md`](internals/development.md) — tooling, the three test rungs, CI, release procedure, add-a-field checklists.
- [`design.md`](internals/design.md) — *historical*: the original design document. Long-form rationale; superseded in specifics by the [ADRs](adr/README.md), which always win.

## By goal

| I want to… | Go to |
|---|---|
| evaluate kube-vnet | [concepts](getting-started/concepts.md), [FAQ](faq.md), the [front-page pitch](../README.md) |
| install or upgrade | [install](getting-started/install.md), [configuration reference](reference/configuration.md) |
| build my first network | [first-vnet](getting-started/first-vnet.md) |
| solve a concrete task | [recipes](guides/recipes.md) |
| understand a policy the operator created on its own | [auto-allow](guides/auto-allow.md) |
| run it in production | [operations](guides/operations.md), [security](guides/security.md), [metrics & events](reference/metrics-and-events.md) |
| fix something broken | [troubleshooting](guides/troubleshooting.md), then [cni-pitfalls](guides/cni-pitfalls.md) if policies exist but don't enforce |
| look up a field / flag / label | [reference/](reference/api.md) |
| contribute | [development](internals/development.md), [architecture](internals/architecture.md), [ADRs](adr/README.md) |
