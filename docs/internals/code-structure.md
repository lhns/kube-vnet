# Code structure

> File-by-file responsibilities and the runtime flow between them. Descriptive, not authoritative: if it disagrees with the code, the code wins — update this doc in the same PR.

## File tree (functionality only, tests excluded)

```
kube-vnet/
│
├── cmd/
│   ├── main.go ............................. operator entrypoint: flags, manager
│   │                                         setup, wires the eight reconcilers,
│   │                                         the metrics collector and (with
│   │                                         --webhook-enabled) the webhooks
│   └── networkwait.go ...................... the network-wait and network-beacon
│                                             subcommands (ADR 0045)
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
├── internal/webhook/podresolution/ ......... optional admission webhooks (ADR 0034)
│   ├── handlers.go ......................... Deps shared by both handlers
│   │                                         (decode, operator and namespace
│   │                                         checks); Register serves them
│   ├── mutator.go .......................... stamps kube-vnet.system/* labels +
│   │                                         resolved-generation/resolved-by
│   │                                         during pod admission
│   ├── validator.go ........................ rejects kube-vnet.system/* label
│   │                                         changes that disagree with
│   │                                         resolution (operator SA exempt)
│   └── networkwait.go ...................... builds the injected network-wait
│                                             init container (ADR 0045)
│
├── internal/networkwait/ ................... the wait (dial every beacon until
│                                             all accept or the max runs out)
│                                             and the beacon listener
│
└── internal/controller/ .................... operator logic
    │
    │   --- Pure functions and shared helpers ---
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
    │                                         annotation); opt-out/opt-in
    │                                         annotation keys
    ├── permits.go ........................... Permits: may a namespace join a
    │                                         vnet (existence +
    │                                         allowedNamespaces)
    ├── resolver.go .......................... Resolver: builds the three layers
    │                                         for a pod and returns the desired
    │                                         kube-vnet.system/* labels; shared
    │                                         by the ResolutionReconciler and
    │                                         the mutating webhook. Hosts
    │                                         canonicalVnetKey
    ├── stamps.go ............................ SyncStamps / MarkResolved /
    │                                         ClearResolved: the one way both
    │                                         the reconciler and the webhook
    │                                         write stamps onto a pod
    ├── sweep.go ............................. sweepStalePolicies: label-scoped
    │                                         deletion of policies not in the
    │                                         desired set
    ├── metrics.go ........................... the six kube_vnet_* metrics and
    │                                         the 30s MetricsCollector
    │
    │   --- Reconcilers (controller-runtime) ---
    ├── resolution_controller.go ............. ResolutionReconciler: watches
    │                                         Pod + Namespace + VirtualNetwork
    │                                         + the two baseline CRDs +
    │                                         VirtualNetworkBinding (ADR 0044:
    │                                         it reads all of them). Calls
    │                                         the Resolver and patches
    │                                         kube-vnet.system/* labels +
    │                                         resolved-generation/resolved-by
    │                                         annotations onto pods. Hosts
    │                                         CanonicalSuffix
    ├── virtualnetwork_controller.go ......... VirtualNetworkReconciler: watches
    │                                         VirtualNetwork + Pod +
    │                                         NetworkPolicy +
    │                                         Namespace. Discovers
    │                                         members (by system label), calls
    │                                         policy_generator.go::Generate,
    │                                         SSA-applies the resulting
    │                                         NetworkPolicies, runs
    │                                         deleteMembershipPolicies tail-step,
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
    │                                         VirtualNetwork + Pod +
    │                                         Namespace. Resolves binding's
    │                                         podSelector, sets binding
    │                                         status (Ready, attachedPods).
    │                                         Does NOT emit policies —
    │                                         bindings stamp via resolution
    │                                         per ADR 0033
    ├── external_allow_controller.go ......... ExternalAllowReconciler:
    │                                         kube-vnet.ext.svc.* (ADR 0038)
    ├── hostport_controller.go ............... HostPortReconciler:
    │                                         kube-vnet.ext.host.* (ADR 0040)
    └── apiserver_reachable_controller.go .... ApiserverReachableReconciler:
                                              kube-vnet.ext.apiserver.*
                                              (ADR 0041)
```

The pod-scoped diagnostics are emitted by the `ResolutionReconciler`: `VirtualNetworkNotJoinable` and `InvalidJoinLabelDirection` through the `Resolver`, `ResolutionConflict` and `OverrideRejected` from the resolution result. The separate `JoinLabelDiagnosticReconciler` was retired (ADR 0027 retirement amendment).

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
                │   1. Resolver.buildLayers() →      │   Binding
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
                  │   kube-vnet.system/resolved-by=controller │
                  │   (the mutating webhook writes the same   │
                  │   set at admission: resolved-by=admission)│
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
              │     by FQ system label across NSes,         │  Namespace
              │     skips pods missing resolved-generation  │
              │     (race-window safety)                    │
              │  2. policy_generator.go::Generate()  ──────►│  pure function:
              │     returns per-(vnet, NS) NetworkPolicy    │  selector +
              │     specs                                   │  peer rules,
              │  3. SSA-apply each policy with              │  cluster vnet
              │     FieldManager="kube-vnet"                │  has bare name
              │  4. deleteMembershipPolicies() — list by    │  per ADR 0033
              │     kube-vnet.system/network=<homeNS>.<vnet>│
              │     label, delete anything not in           │
              │     desired set (hard cleanup, ADR 0033)    │
              │  5. updateStatus — Ready / Degraded         │
              └────┬────────────────────────────────────────┘
                   │ SSA apply
                   ▼
   ┌───────────────────────────────────────────────────────────────┐
   │  NetworkPolicy: kube-vnet.mem.<homeNS>.<vnet>-<8hex> (one per │
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

```

## The narrow waist

The system label `kube-vnet.system/net.<canonical-key>=<direction>` is the contract between input and output:

- **Writers**: the resolution controller, and the mutating webhook when enabled — both through the same `Resolver`.
- **Reader**: the policy generator (via `NetworkPolicy` `matchExpressions`).
- **Guard**: a chart-shipped `ValidatingAdmissionPolicy` lets only the operator's ServiceAccount change these labels; with the webhook enabled, the validating webhook takes over for the pods it sees and additionally checks values against resolution; the policy still covers the pods the webhooks skip.

## Reconciler boundaries

Each reconciler owns exactly one resource family and never crosses into another's territory:

Per [ADR 0044](../adr/0044-trigger-sets-must-cover-read-sets.md) the third column is
also the **trigger** set: every input a reconciler reads it must also watch, because
change-based predicates filter the informer resync, so an unwatched input diverges
permanently rather than slowly.

| Reconciler | Writes | Reads = watches |
|---|---|---|
| `VirtualNetworkReconciler` | `NetworkPolicy` (membership), vnet `status` | `VirtualNetwork`, `Pod` (system labels), `NetworkPolicy` (drift), `Namespace` |
| `NamespaceReconciler` | `NetworkPolicy` (baseline) | `Namespace`, baseline `NetworkPolicy` (drift) |
| `ResolutionReconciler` | `Pod` labels + annotations; `VirtualNetworkNotJoinable` Events on the declaring object; `InvalidJoinLabelDirection`, `ResolutionConflict`, `OverrideRejected` Events on the pod | `Pod`, `Namespace` (annotation + labels), `VirtualNetwork`, `ClusterVirtualNetworkBaseline`, `VirtualNetworkBaseline`, `VirtualNetworkBinding` |
| `SystemVnetReconciler` | `VirtualNetwork` (the `namespace` and `cluster` singletons) | `Namespace`, `VirtualNetwork` (drift) |
| `VirtualNetworkBindingReconciler` | `VirtualNetworkBinding` `status` | `VirtualNetworkBinding`, `VirtualNetwork`, `Pod`, `Namespace` |
| `ExternalAllowReconciler` | `NetworkPolicy` (`ext.svc`), `Pending`/`Skipped` Events | `Service`, `Namespace`, `Pod` (entering or leaving a named-port Service's selector), own policies (drift) |
| `HostPortReconciler` | `NetworkPolicy` (`ext.host`) | `Namespace`, `Pod` (hostPort changes), own policies (drift) |
| `ApiserverReachableReconciler` | `NetworkPolicy` (`ext.apiserver`), `Pending` Events | `Service`, `Namespace`, `Pod` (entering or leaving a named-port Service's selector), own policies (drift), Validating/MutatingWebhookConfiguration, `APIService`, `CustomResourceDefinition` |

The pure-function split (`resolution.go`, `policy_generator.go`, `baseline.go`) keeps the I/O-driven logic in the controllers thin and easy to unit-test against contrived inputs.
