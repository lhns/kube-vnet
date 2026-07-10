# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately via GitHub's private vulnerability reporting:

**https://github.com/lhns/kube-vnet/security/advisories/new**

Please include:

- what an attacker gains (e.g. "a namespace tenant can reach pods in another namespace")
- the kube-vnet version, Kubernetes version, and CNI
- a minimal reproduction — manifests and the observed vs expected connectivity

You will get an acknowledgement, and we will coordinate a fix and disclosure timeline with you. If you would like credit in the release notes, say so.

## Supported versions

kube-vnet is pre-1.0. Only the **latest released minor** receives security fixes. The API is `v1alpha1` and breaking changes are possible in any release — pin an exact version.

## What counts as a vulnerability

kube-vnet generates `networking.k8s.io/v1` NetworkPolicy objects from a membership model. Its security boundary is the correctness of that generation. Treat as reportable:

- a pod obtaining **ingress** it should not have, without the vnet owner authorizing it
- a namespace tenant **self-granting** membership in another namespace's VirtualNetwork
- forging or bypassing the operator-owned `kube-vnet.system/*` labels on a cluster where the ValidatingAdmissionPolicies are installed (Kubernetes ≥ 1.30)
- privilege escalation via the operator's or the Helm cleanup hook's ServiceAccount
- supply-chain issues in released images or charts

## Known non-issues (documented, by design)

These are analysed in [`docs/security/threat-model.md`](docs/security/threat-model.md) and are **not** vulnerabilities. Reports of them will be closed with a pointer here.

- **Egress is not restricted.** kube-vnet's baseline is ingress-only ([ADR 0025](docs/adr/0025-ingress-isolation-rename-egress-unrestricted.md)). It does not defend against exfiltration from a compromised pod.
- **A non-enforcing CNI makes every policy decorative.** kube-vnet writes policy; the CNI enforces it. See [`docs/guides/cni-pitfalls.md`](docs/guides/cni-pitfalls.md).
- **Cluster-admin can defeat kube-vnet**, as can anyone who can stop the operator or edit its RBAC.
- **A namespace owner with `patch namespace` can opt out** via `kube-vnet/disabled=true`, and one with `delete networkpolicy` can delete the baseline (restored within a reconcile cycle — a race, not a boundary).
- **On Kubernetes < 1.30 the ValidatingAdmissionPolicies do not install** (VAP is GA in 1.30). The chart warns at install time.
- **Established connections survive a policy tightening.** NetworkPolicy is enforced at SYN time; conntrack does not re-evaluate. Universal to Kubernetes.
- **hostNetwork pods are outside the model** — NetworkPolicy generally does not apply to them.
- **Auto-allow families intentionally open `0.0.0.0/0`** on ports you exposed yourself (LoadBalancer/NodePort Services, hostPort pods, apiserver-dialed webhooks). Opt out with `kube-vnet/external-allow=false`; narrow the webhook family with `--apiserver-source-cidr`.

## Supply chain

Released container images and Helm charts are signed with [cosign](https://github.com/sigstore/cosign) (keyless, GitHub OIDC) and ship SPDX SBOMs as cosign attestations and release assets. Verification instructions are in [`docs/security/security.md`](docs/security/security.md#supply-chain).
