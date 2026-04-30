# 0008 — Pure-function policy generator

Status: Accepted

## Context

The operator's most important responsibility is producing the right `NetworkPolicy` set for a given (VirtualNetwork, member set). The mistake-prone parts are selector keys, peer enumeration across namespaces, and DNS allowance — all of which are functional, not stateful.

Mixing this logic into the reconciler (which also does I/O against the API server) makes it tedious to test and easy to break inadvertently.

## Decision

The policy generator is a **pure function**:

```go
// in internal/controller/policy_generator.go
func Generate(in GenerateInput) GenerateOutput
```

Inputs: the `VirtualNetwork`, the configured label prefix, and the pre-computed member set (`map[namespace][]podName`). Outputs: the desired `[]NetworkPolicy`.

The generator does no I/O. It does not know about the Kubernetes client, the cache, or the field manager. The reconciler is responsible for membership discovery and applying the result.

## Consequences

- **Pro**: Trivially unit-testable — every branch (no members, single namespace, multi-namespace, name truncation, DNS allowance) has table-driven coverage in `policy_generator_test.go`.
- **Pro**: The generator's surface is small; reviewers can read it end-to-end.
- **Pro**: Refactoring the reconciler doesn't risk breaking policy correctness, and vice versa.
- **Con**: Membership discovery (the only side-effect-laden part) is in the reconciler and harder to unit-test in isolation. envtest covers this end-to-end (ADR 0014 / deferred).
