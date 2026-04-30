# 0017 — Name validation via CEL and runtime check

Status: Accepted

## Context

The label encoding chosen in ADR 0004 uses dots as separators between namespace and VirtualNetwork name (`kube-vnet/net.<homeNS>.<vnetName>`). If a VirtualNetwork's name contained a dot, the encoding would be ambiguous: the operator could not tell where the namespace ends and the name begins.

Kubernetes' default validation for resource `metadata.name` on CRDs is **DNS-1123 subdomain**, which *allows* dots. So a user could create `apiVersion: kube-vnet/v1alpha1, kind: VirtualNetwork, metadata.name: payments.v2` and the apiserver would accept it. The operator would then produce broken policy selectors.

We need to reject names with dots at admission time and (defense-in-depth) handle the case where one slips through anyway.

## Decision

Two layers:

### 1. CRD-level CEL validation (admission-time)

The CRD declares an `x-kubernetes-validations` rule on the root schema:

```go
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="VirtualNetwork name must be a DNS-1123 label (lowercase alphanumeric and hyphens; no dots)"
```

This compiles into:

```yaml
x-kubernetes-validations:
  - rule: self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')
    message: VirtualNetwork name must be a DNS-1123 label …
```

The apiserver evaluates the rule at admission time. Names with dots are rejected with a clear error before they're persisted.

### 2. Runtime defense-in-depth

The reconciler keeps a regex (`nameRegex`) and validates `vnet.Name` at the top of `Reconcile`. If validation fails (e.g. an old object existed before the CEL rule, or a future bug bypasses admission), the reconciler:

- Sets `Ready=False, reason=InvalidName`.
- Sets `Degraded=True, reason=InvalidName` with a message naming the regex.
- Emits no policies.

This way an invalid object surfaces visibly rather than silently producing broken policy selectors.

### What about a validating admission webhook?

A webhook would offer richer validation (e.g. semantic checks across spec fields), but:

- It requires TLS cert plumbing (cert-manager or self-signed bootstrapping).
- It adds a deployment dependency and another failure mode (webhook unavailable → API rejects).
- The only known invalid case is "name contains a dot", which CEL covers.

A webhook is not added in v1. If future validation needs exceed what CEL can express, this ADR can be superseded.

## Consequences

- **Pro**: Invalid names cannot be persisted; user gets immediate, clear feedback at `kubectl apply` time.
- **Pro**: Belt-and-braces — even if admission is bypassed, the operator surfaces the problem instead of producing broken policies.
- **Pro**: No webhook, no certs, no extra moving parts.
- **Con**: CEL requires Kubernetes 1.25+ (1.29+ for stable). The project targets a current Kubernetes baseline so this is acceptable.
- **Con**: A user who carefully crafts an existing object via direct etcd write or other unusual path can still create one — at which point the runtime check catches it. No silent failure mode.
