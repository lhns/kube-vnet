# Code structure

> Snapshot of the operator's Go source layout — file-by-file responsibilities and the runtime flow between them.
>
> **As of commit** [`12d9f63`](../../../../commit/12d9f63) — *remove --elide-baseline-for: baseline elide had no observable effect (ADR 0035)*.
>
> This document is descriptive, not authoritative. If it disagrees with the code, the code wins; update this doc in the same PR.

## File tree (functionality only, tests excluded)

```
kube-vnet/
│
├── cmd/
│   └── main.go ............................. operator entrypoint: flags, manager
│                                             setup, instantiates and wires all
│                                             five reconcilers
│
├── api/v1alpha1/ ........................... CRD type definitions (kubebuilder)
│   ├── groupversion_info.go ................ Go package marker, GroupVersion
│   │                                         registration for the scheme
│   ├── virtualnetwork_types.go ............. VirtualNetwork CRD: home-NS scoped
│   │                                         named-network, allowedNamespaces,
│   │                                         status conditions
│   ├── virtualnetworkbinding_types.go ...... VirtualNetworkBinding CRD: per-NS
│   │                                         pod-selector → vnet attach (no-label
│   │                                         alternative; ADR 0026)
│   ├── virtualnetworkbaseline_types.go ..... VirtualNetworkBaseline CRD: per-NS
│   │                                         tier defaults that override
│   │                                         ClusterVirtualNetworkBaseline
│   │                                         (ADR 0031)
│   └── clustervirtualnetworkbaseline_types.go  ClusterVirtualNetworkBaseline CRD:
│                                             cluster-wide tier defaults
│                                             (ADR 0031)
│
└── internal/controller/ .................... operator logic
    │
    │   --- Pure / library code (no I/O) ---
    ├── resolution.go ....................... pure resolver: takes ordered
    │                                         (cluster → ns → pod) layers of
    │                                         (vnet, direction) rules and
    │                                         computes ResolutionResult
    │                                         (Effective + Conflicts +
    │                                         OverrideRejected). The
    │                                         intersection-truth-table /
    │                                         default-* override permissions
    │                                         live here (ADR 0030/0031)
    ├── policy_generator.go ................. pure NetworkPolicy generator:
    │                                         given a vnet and member sets,
    │                                         emits the per-(vnet, NS) ingress
    │                                         policy with FQ system-label
    │                                         selector and peer rules. Hosts
    │                                         Direction enum, ParseDirection,
    │                                         PolicyName (with cluster
    │                                         singleton special-case per
    │                                         ADR 0033)
    ├── baseline.go .......................... pure DesiredBaseline: the
    │                                         uniform deny-all PodSelector:{}
    │                                         baseline NetworkPolicy spec
    │                                         (post-ADR-0035, no elide knob)
    ├── namespace.go ......................... NamespaceFilter (managed vs
    │                                         disabled namespace check via
    │                                         --disabled-namespaces flag +
    │                                         kube-vnet/disabled=true
    │                                         annotation)
    ├── metrics.go ........................... Prometheus metric registration
    │                                         (apply-error counter, reconcile
    │                                         counter, resolution conflicts,
    │                                         membership policy size)
    │
    │   --- Reconcilers (controller-runtime) ---
    ├── resolution_controller.go ............. ResolutionReconciler: watches
    │                                         Pod + the two baseline CRDs +
    │                                         VirtualNetworkBinding. Builds
    │                                         the three resolution layers,
    │                                         calls resolution.go::Resolve,
    │                                         patches kube-vnet.system/net.*
    │                                         labels + resolved-generation
    │                                         annotation onto pods. Hosts
    │                                         CanonicalSuffix and
    │                                         canonicalVnetKey
    ├── virtualnetwork_controller.go ......... VirtualNetworkReconciler: watches
    │                                         VirtualNetwork + Pod +
    │                                         NetworkPolicy +
    │                                         VirtualNetworkBinding. Discovers
    │                                         members (by system label), calls
    │                                         policy_generator.go::Generate,
    │                                         SSA-applies the resulting
    │                                         NetworkPolicies, runs
    │                                         deleteStale cleanup tail-step,
    │                                         maintains vnet status conditions
    │                                         (Ready / Degraded)
    ├── namespace_reconciler.go .............. NamespaceReconciler: watches
    │                                         Namespace + baseline
    │                                         NetworkPolicy (drift). Calls
    │                                         baseline.go::DesiredBaseline,
    │                                         SSA-applies; deletes baseline
    │                                         in disabled namespaces
    ├── system_vnet_controller.go ............ SystemVnetReconciler: ensures
    │                                         the per-NS `namespace` system
    │                                         vnet and the operator-NS
    │                                         `cluster` system vnet exist
    │                                         (ADR 0030); deletes the per-NS
    │                                         one on managed→disabled
    │                                         transition
    ├── virtualnetworkbinding_controller.go .. VirtualNetworkBindingReconciler:
    │                                         watches VirtualNetworkBinding +
    │                                         Pod. Resolves binding's
    │                                         podSelector, sets binding
    │                                         status (Ready,
    │                                         attachedPods). Does NOT emit
    │                                         policies — bindings stamp via
    │                                         resolution layer per ADR 0033
    └── joinlabel_diagnostic_controller.go ... JoinLabelDiagnosticReconciler:
                                              emits Warning Events on pods
                                              with misconfigured kube-vnet/
                                              net.* labels (typos, dangling
                                              vnet refs, NS-not-allowed)
                                              per ADR 0027
```

## Code flow

Two flows: **input side** (CRDs/pods → stamped pod labels) and **output side** (vnet + stamped pods → NetworkPolicies). The system label `kube-vnet.system/net.<key>` is the contract between them.

```
                          === INPUT SIDE: resolution ===

   ClusterVNetBaseline        VirtualNetworkBaseline           Pod
   ┌──────────────────┐       ┌───────────────────────┐       ┌────────────────┐
   │  cluster: both   │       │  payments: ingress    │       │ labels:        │
   │  namespace: both │       │  (per-NS overrides)   │       │  kube-vnet/    │
   └────────┬─────────┘       └──────────┬────────────┘       │   net.foo=both │
            │                            │                    └────────┬───────┘
            │                            │                             │
            └──────────────┬─────────────┴───────────────┬─────────────┘
                           │                             │
                           ▼                             ▼
                  VirtualNetworkBinding       (binding controller updates
                  ┌──────────────────┐         binding.status only —
                  │  podSelector +   │         no policy emission)
                  │  vnet ref +      │
                  │  direction       │         virtualnetworkbinding_controller.go
                  └──────────┬───────┘
                             │
                             ▼
                ┌────────────────────────────────────┐
                │   resolution_controller.go         │   watches: Pod (label
                │   .Reconcile(pod)                  │   change), ClusterVNB,
                │                                    │   VNB, VirtualNetwork
                │   1. buildLayers() →               │   Binding
                │      [cluster, ns, pod-tier]       │
                │   2. resolution.go::Resolve()  ───►│ pure function:
                │      returns Effective +           │   intersection-truth-table,
                │      Conflicts + Rejections        │   default-* override perms
                │   3. canonicalVnetKey /            │
                │      CanonicalSuffix (cluster      │
                │      collapses bare per ADR 0033)  │
                │   4. applyResolution() ──┐         │
                └──────────────────────────┼─────────┘
                                           │ patch pod metadata
                                           ▼
                  ┌───────────────────────────────────────────┐
                  │ Pod labels:                               │
                  │   kube-vnet.system/net.<canonical>=<dir>  │
                  │ Pod annotations:                          │
                  │   kube-vnet.system/resolved-generation=N  │
                  └─────────────────────┬─────────────────────┘
                                        │
                                        │  (this label is the contract;
                                        │   the output side reads it back)
                                        ▼

                         === OUTPUT SIDE: policy emission ===

      VirtualNetwork                   Pod (with system labels stamped)
      ┌──────────────────┐             ┌────────────────────────────┐
      │  spec.allowed    │             │ kube-vnet.system/          │
      │   Namespaces     │             │   net.platform.payments=   │
      │                  │             │   both                     │
      └────────┬─────────┘             └────────────┬───────────────┘
               │                                    │
               └──────────────────┬─────────────────┘
                                  │
                                  ▼
              ┌─────────────────────────────────────────────┐
              │  virtualnetwork_controller.go               │  watches:
              │  .Reconcile(vnet)                           │  VirtualNetwork,
              │                                             │  Pod, NetworkPolicy
              │  1. discoverMembers(vnet) — lists pods      │  (drift),
              │     by FQ system label across NSes,         │  VirtualNetwork
              │     skips pods missing resolved-generation  │  Binding
              │     (race-window safety)                    │
              │  2. policy_generator.go::Generate()  ──────►│  pure function:
              │     returns per-(vnet, NS) NetworkPolicy    │  selector +
              │     specs                                   │  peer rules,
              │  3. SSA-apply each policy with              │  cluster vnet
              │     FieldManager="kube-vnet"                │  has bare name
              │  4. deleteStale() — list policies by        │  per ADR 0033
              │     kube-vnet.system/network=<homeNS>.<vnet>       │
              │     label, delete anything not in           │
              │     desired set (hard cleanup, ADR 0033)    │
              │  5. updateStatus — Ready / Degraded         │
              └────┬────────────────────────────────────────┘
                   │ SSA apply
                   ▼
   ┌───────────────────────────────────────────────────────────────┐
   │  NetworkPolicy: kube-vnet.<homeNS>.<vnet>-<8hex>  (one per    │
   │                                                    member-NS) │
   │  podSelector:                                                 │
   │    matchExpressions: [{key: kube-vnet.system/net.<canonical>, │
   │                        operator: In, values: [both, ingress]}]│
   │  ingress[0].from: [<peer NSes podSelector each>]              │
   │  policyTypes: [Ingress]                                       │
   └───────────────────────────────────────────────────────────────┘

                       === BASELINE (orthogonal lifecycle) ===

                                Namespace event
                                       │
                                       ▼
              ┌─────────────────────────────────────────┐
              │  namespace_reconciler.go                │
              │  .Reconcile(ns)                         │
              │                                         │
              │   IsManaged? ──no──► delete baseline    │
              │      │                                  │
              │      yes                                │
              │      ▼                                  │
              │   baseline.go::DesiredBaseline(ns)  ───►│  PodSelector: {}
              │   SSA-apply                             │  policyTypes: [Ingress]
              └─────────────────────────────────────────┘  no allow rules =
                                                           deny-all floor

                       === SYSTEM VNETS (auto-created) ===

                                Namespace event
                                       │
                                       ▼
              ┌─────────────────────────────────────────┐
              │  system_vnet_controller.go              │
              │  .Reconcile(ns)                         │
              │                                         │
              │   IsManaged? ─yes─► ensure `namespace`  │
              │                      vnet exists in ns  │
              │   ns == OperatorNS? → ensure `cluster`  │
              │                       vnet exists       │
              │   IsManaged transitioned to disabled?   │
              │       → delete the per-NS namespace vnet│
              └─────────────────────────────────────────┘

                       === DIAGNOSTICS (no enforcement) ===

                              Pod event with kube-vnet/net.* label
                                       │
                                       ▼
              ┌─────────────────────────────────────────┐
              │  joinlabel_diagnostic_controller.go     │
              │  validates: vnet exists, NS allowed,    │
              │             direction value valid       │
              │  emits Warning Events on the pod        │
              │  (does not patch labels)                │
              └─────────────────────────────────────────┘
```

## The narrow waist

The system label `kube-vnet.system/net.<canonical-key>=<direction>` is the contract between input and output:

- **Resolution controller** is the only writer.
- **Policy generator** and **baseline** are the only readers (via `NetworkPolicy` `matchExpressions`).
- A `ValidatingAdmissionPolicy` (chart-shipped, not Go code) blocks user mutation of these labels — only the operator's ServiceAccount can write them.

## Reconciler boundaries

Each reconciler owns exactly one resource family and never crosses into another's territory:

| Reconciler | Writes | Reads (for decisions) |
|---|---|---|
| `VirtualNetworkReconciler` | `NetworkPolicy` (membership), vnet `status` | `VirtualNetwork`, `Pod` (system labels), `VirtualNetworkBinding` |
| `NamespaceReconciler` | `NetworkPolicy` (baseline) | `Namespace`, baseline `NetworkPolicy` (drift) |
| `ResolutionReconciler` | `Pod` labels + annotations | `Pod`, `ClusterVirtualNetworkBaseline`, `VirtualNetworkBaseline`, `VirtualNetworkBinding` |
| `SystemVnetReconciler` | `VirtualNetwork` (the `namespace` and `cluster` singletons) | `Namespace`, `VirtualNetwork` (drift) |
| `VirtualNetworkBindingReconciler` | `VirtualNetworkBinding` `status` | `VirtualNetworkBinding`, `Pod` |
| `JoinLabelDiagnosticReconciler` | Kubernetes `Event` on pods | `Pod`, `VirtualNetwork` |

The pure-function split (`resolution.go`, `policy_generator.go`, `baseline.go`) keeps the I/O-driven logic in the controllers thin and easy to unit-test against contrived inputs.
