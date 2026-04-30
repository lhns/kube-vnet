# kube-vnet documentation

Where to start, by what you're trying to do.

## I want to evaluate kube-vnet

- [**Concepts**](concepts.md) — the model in depth: VirtualNetworks, direction modes, the join-eligibility rule for `allowedNamespaces`, the ingress-isolation baseline, `VirtualNetworkBinding` as the no-label alternative, how it all maps onto stock NetworkPolicy.
- [**FAQ**](faq.md) — common upfront questions: production-ready?, CNI compatibility, comparison to AdminNetworkPolicy, etc.
- [`../README.md`](../README.md) — short pitch + worked example on the project front page.

## I want to install or upgrade

- [**Install**](install.md) — Helm install (recommended), `kubectl apply` install, verifying signatures and SBOMs, upgrade procedure, uninstall.
- [**Reference / Configuration**](reference/configuration.md) — every operator flag, every Helm value.

## I'm using it day-to-day

- [**Concepts**](concepts.md) — the mental model that makes the API obvious.
- [**Recipes**](recipes.md) — worked examples beyond the minimal samples in `config/samples/`: three-tier app, observability network, bridge pods, direction-mode patterns, `VirtualNetworkBinding`, migration, coexisting with user-managed NetworkPolicy, per-workload egress allowlists.
- [**Reference / API**](reference/api.md) — full `VirtualNetwork` and `VirtualNetworkBinding` CRD specs + the status condition taxonomy.
- [**Reference / Labels & annotations**](reference/labels-and-annotations.md) — every label and annotation kube-vnet writes or honors.

## I'm running it in production

- [**Operations**](operations.md) — deployment topology, leader election, sizing, monitoring, sample Prometheus alerts.
- [**Security**](security.md) — threat model, RBAC inventory, supply-chain (cosign + SBOM + Trivy), hardening, what kube-vnet does *not* defend.
- [**Reference / Metrics & events**](reference/metrics-and-events.md) — every metric and every Kubernetes Event the operator emits.

## Something is broken

- [**Troubleshooting**](troubleshooting.md) — symptom → diagnosis. The starting point when "my pod isn't a member" or "pods can talk that shouldn't."

## I want to contribute

- [**Development**](development.md) — code layout, the three test rungs (unit / envtest / kind+CNI), release procedure.
- [**Architecture**](architecture.md) — internals: the two reconcilers, the pure-function policy generator, watches, drift correction, baseline lifecycle.
- [**ADRs**](adr/README.md) — Architecture Decision Records. Every significant decision lives here.

## Reference material (look-ups, not narrative)

- [`reference/api.md`](reference/api.md) — `VirtualNetwork` and `VirtualNetworkBinding` CRDs: every field, every status condition reason.
- [`reference/configuration.md`](reference/configuration.md) — every flag, every Helm value, every env var.
- [`reference/labels-and-annotations.md`](reference/labels-and-annotations.md) — the kube-vnet label/annotation contract.
- [`reference/metrics-and-events.md`](reference/metrics-and-events.md) — observability surface.

## Background

- [`kube-vnet-design.md`](kube-vnet-design.md) — the original design document. Long-form rationale for the project. Where the design doc and the implementation disagree, the [ADRs](adr/README.md) win.
- [`adr/`](adr/README.md) — every accepted decision, in MADR-lite format.

## Glossary

- [`glossary.md`](glossary.md) — defined terms. `vnet`, `member`, `home namespace`, `bare label`, etc.
