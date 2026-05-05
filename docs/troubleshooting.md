# Troubleshooting

Symptom-first. Look up what you're seeing in the table of contents, follow the diagnostic steps.

For the full list of status-condition reasons and what each one means, see [`reference/api.md`](reference/api.md). For metrics and events, [`reference/metrics-and-events.md`](reference/metrics-and-events.md).

---

## Index

- [Pod events kube-vnet emits](#pod-events-kube-vnet-emits)
- [`kubectl apply` rejected my pod: "value not in [both, ingress, egress, none, true, false]"](#kubectl-apply-rejected-my-pod-value-not-in-both-ingress-egress-none-true-false)
- [My pod with `kube-vnet/net.X: "true"` (or `""`/`"false"`) stopped working after upgrade](#my-pod-with-kube-vnetnetx-true-or-false-stopped-working-after-upgrade)
- [My pod has the join label but isn't a member](#my-pod-has-the-join-label-but-isnt-a-member)
- [Pods I expect to be isolated can talk to each other](#pods-i-expect-to-be-isolated-can-talk-to-each-other)
- [CNI pitfalls that silently break enforcement (separate page)](troubleshooting/cni-pitfalls.md)
- [Egress to the public internet just started working after upgrade](#egress-to-the-public-internet-just-started-working-after-upgrade)
- [The default-deny baseline didn't appear](#the-default-deny-baseline-didnt-appear)
- [The baseline disappeared after I deleted my vnet — bug?](#the-baseline-disappeared-after-i-deleted-my-vnet--bug)
- [My VirtualNetworkBinding doesn't attach any pods](#my-virtualnetworkbinding-doesnt-attach-any-pods)
- [Binding shows Ready=False, NamespaceNotAllowed](#binding-shows-readyfalse-namespacenotallowed)
- [Degraded with UnknownDirection or ConflictingDirections](#degraded-with-unknowndirection-or-conflictingdirections)
- ["kubectl get vnet" shows READY=False](#kubectl-get-vnet-shows-readyfalse)
- ["Degraded" condition is True — what does each reason mean?](#degraded-condition-is-true--what-does-each-reason-mean)
- [Operator logs are noisy with conflict / "object has been modified" errors](#operator-logs-are-noisy-with-conflict--object-has-been-modified-errors)
- [Operator logs show "is being terminated" errors](#operator-logs-show-is-being-terminated-errors)
- [I see "PolicyRestored" Warning events — is something wrong?](#i-see-policyrestored-warning-events--is-something-wrong)
- [The operator pod won't start](#the-operator-pod-wont-start)
- [How do I tell whether the operator is healthy?](#how-do-i-tell-whether-the-operator-is-healthy)
- [Useful inspection commands](#useful-inspection-commands)

---

## Pod events kube-vnet emits

Three Warning events fire on the Pod itself when a `kube-vnet/net.*` label is present but the membership can't be honored. They surface in `kubectl describe pod` and via `kubectl get events --field-selector involvedObject.kind=Pod`. See [ADR 0027](adr/0027-pod-scoped-join-label-events.md) for the design.

> Pods in a `kube-vnet/disabled=true` (or `--disabled-namespaces`) namespace do not get these events. Disabled is an explicit opt-out — the operator stays silent there by design.

### `BareJoinLabelVnetNotFound`

**Symptom.** The pod has `kube-vnet/net.<X>` (bare form) and isn't a member. `kubectl describe pod` shows:

```
Events:
  Type     Reason                        Age   From       Message
  ----     ------                        ----  ----       -------
  Warning  BareJoinLabelVnetNotFound     10s   kube-vnet  no VirtualNetwork named "X" in namespace "<this-pod-ns>"
```

**Cause.** Either no `VirtualNetwork` of name `<X>` exists in the pod's *own* namespace, or the pod is in a foreign namespace where the bare form is not recognized at all. The bare form only works in the vnet's home namespace.

**Fix.** If the vnet lives in a different namespace, use the prefixed form:

```yaml
labels:
  kube-vnet/net.<home-ns>.<vnet-name>: both   # not kube-vnet/net.<vnet-name>
```

If the vnet really is meant to live in the pod's namespace, create it:

```yaml
apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: <X>
  namespace: <this-pod-ns>
```

### `PrefixedJoinLabelVnetNotFound`

**Symptom.** The pod has `kube-vnet/net.<homeNS>.<X>` and isn't a member. `kubectl describe pod`:

```
Events:
  Type     Reason                            Age   From       Message
  ----     ------                            ----  ----       -------
  Warning  PrefixedJoinLabelVnetNotFound     10s   kube-vnet  no VirtualNetwork "<homeNS>/<X>"
```

**Cause.** The vnet `<homeNS>/<X>` doesn't exist — either typo'd home-namespace, typo'd vnet name, or the vnet hasn't been created yet.

**Fix.** Verify the vnet exists at the named home:

```bash
kubectl get vnet -n <homeNS> <X>
```

Either correct the label key, or apply the missing `VirtualNetwork` manifest.

### `JoinLabelNamespaceNotAllowed`

**Symptom.** The pod has `kube-vnet/net.<homeNS>.<X>`, the vnet `<homeNS>/<X>` exists, and the pod still isn't a member. `kubectl describe pod`:

```
Events:
  Type     Reason                          Age   From       Message
  ----     ------                          ----  ----       -------
  Warning  JoinLabelNamespaceNotAllowed    10s   kube-vnet  vnet "<homeNS>/<X>" does not allow namespace "<this-pod-ns>"
```

**Cause.** The vnet's `spec.allowedNamespaces` does not permit the pod's namespace. Same condition that surfaces on the *vnet's* `Degraded`/`InvalidJoiners` status, but addressed to the pod owner instead of the vnet owner.

**Fix.** Either extend the vnet's `allowedNamespaces`:

```bash
kubectl patch vnet -n <homeNS> <X> --type=merge -p '
spec:
  allowedNamespaces:
    names: [<this-pod-ns>]
'
```

Or move the pod to a permitted namespace. (The vnet owner has to make the policy decision; pod owners can't override `allowedNamespaces`.)

---

## `kubectl apply` rejected my pod: "value not in [both, ingress, egress, none, true, false]"

```
$ kubectl apply -f mypod.yaml
The pods "my-pod" is invalid: ValidatingAdmissionPolicy "kube-vnet-direction-values"
denied request: kube-vnet/net.* label value must be one of [both ingress egress none true false ""]
```

**Cause.** Kubernetes ≥ 1.30 with the kube-vnet chart installed runs a `ValidatingAdmissionPolicy` that rejects Pod create/update when any `kube-vnet/net.*` label has an unrecognized value (typo like `bothh`, or an arbitrary string). See [ADR 0027](adr/0027-pod-scoped-join-label-events.md).

**Fix.** Set the value to one of the recognized direction strings:

```yaml
labels:
  kube-vnet/net.payments: both        # bidirectional, the usual choice
  # or `ingress`, `egress`, `none`
```

The legacy `true`/`false`/empty-string aliases are no longer accepted (dropped per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md); see the [ADR 0021 2026-05-05 addendum](adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-05--legacy-truefalseempty-aliases-dropped)). On clusters older than 1.30 the VAP is skipped (the chart conditions on `apiVersions` discovery). The same typo there is admitted but excluded from membership at reconcile time, surfacing as `Degraded`/`UnknownDirection` on the vnet — see [the section below](#degraded-with-unknowndirection-or-conflictingdirections).

---

## My pod with `kube-vnet/net.X: "true"` (or `""`/`"false"`) stopped working after upgrade

**Symptom.** A pod manifest that used `kube-vnet/net.<vnet>: "true"` (or `""`, or `"false"`) is now rejected at admission, or excluded from membership on older clusters.

**Cause.** Breaking change. The legacy aliases `true`, `false`, and the empty-string value were dropped per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md). The valid direction values are now exactly `both`, `ingress`, `egress`, `none`.

**Fix.** Set an explicit value:

```yaml
labels:
  kube-vnet/net.payments: both     # was: "true" or ""
  # or `none`               (was: "false" or "")
```

---

## My pod has the join label but isn't a member

Most common case. Walk through these in order:

1. **Did you use the right label form?**
   - Pod in the VirtualNetwork's home namespace → either form works: `kube-vnet/net.<vnet-name>=both` or `kube-vnet/net.<home-ns>.<vnet-name>=both`.
   - Pod in *any other* namespace → prefixed only: `kube-vnet/net.<home-ns>.<vnet-name>=both`.

   The pod's namespace decides which forms are valid, not the vnet's.

   **Direction value.** The label value matters now: `both` (default), `ingress`, `egress`, `none`. The legacy `true`/`false`/empty-string aliases were dropped per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md). An unknown value (e.g. a typo `"bothh"`) is rejected and surfaces on the vnet's `Degraded` condition with reason `UnknownDirection`.

2. **Is the pod's namespace operator-excluded?**

   ```bash
   kubectl describe vnet -n <home-ns> <vnet-name> | grep -A4 Conditions:
   ```

   If `Degraded=True` with reason `InvalidJoiners` and the message names your pod's namespace, the namespace is excluded.

   Two ways a namespace can be excluded:
   - The operator-level `--disabled-namespaces` flag (default `kube-system,kube-public,kube-node-lease`, plus the operator's own namespace).
   - The per-namespace annotation: `kubectl get ns <name> -o jsonpath='{.metadata.annotations.kube-vnet/disabled}'` — if it reads `true`, that's why.

3. **Is the pod's namespace permitted by `allowedNamespaces`?**

   ```bash
   kubectl get vnet -n <home-ns> <vnet-name> -o yaml | yq .spec.allowedNamespaces
   ```

   If `allowedNamespaces` is unset, only the home namespace can join. If it's `names: [...]`, your namespace must be in that list (exact match — no globs). If it's `selector: {...}`, your namespace's labels must match.

   A pod in a non-permitted namespace shows up as `Degraded=True, reason=InvalidJoiners` with `NamespaceNotAllowed` in the per-pod reason.

4. **Is the operator alive?**

   ```bash
   kubectl get deploy -n kube-vnet-system kube-vnet-controller
   kubectl get lease -n kube-vnet-system kube-vnet.lhns.de \
     -o jsonpath='{.spec.holderIdentity} {.spec.renewTime}{"\n"}'
   ```

   `renewTime` should be within the last few seconds. If it's stale, the leader has died and no replica took over.

5. **Is the vnet `Ready=True`?**

   ```bash
   kubectl get vnet -n <home-ns> <vnet-name>
   ```

   If `Ready=False`, look at the reason — see [the next section](#kubectl-get-vnet-shows-readyfalse).

---

## I labeled my pod and the vnet is Ready, but external pods can still reach it

The deny-all baseline excludes pods that are *receivers* on any vnet listed in `--elide-baseline-for` (default `cluster`). If your pod is on the cluster system-vnet as `both`/`ingress` (e.g. because `operator.clusterBaseline.ingressIsolationLevel=cluster` seeds `cluster=default-both`), the baseline doesn't apply to it — the cluster-vnet membership policy alone governs its ingress, which by convention allows from anywhere on the cluster.

To enforce stricter ingress on a specific pod:

- Don't make the pod a `cluster=both` member. Either remove the operator default or set `kube-vnet/net.cluster=none` on the pod.
- The deny-all baseline will then apply, and only the user-vnet membership policies grant ingress.

For the full design, see the [deny-all baseline section in `concepts.md`](concepts.md#the-deny-all-baseline) and [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md).

## Pods I expect to be isolated can talk to each other

1. **Does your CNI enforce NetworkPolicy?**

   This is the #1 cause when "all the YAMLs look right but pods talk anyway." kube-vnet generates `NetworkPolicy`; your CNI is what drops packets. If the CNI doesn't enforce, the policies are decorative.

   See [`install.md`](install.md#cni-that-enforces-networkpolicy) for compatible CNIs. Quick check: install Calico/Cilium/kube-router and re-test. If isolation now works, the previous CNI didn't enforce NetworkPolicy.

   If your CNI *claims* to enforce NetworkPolicy and isolation still doesn't work, see [`troubleshooting/cni-pitfalls.md`](troubleshooting/cni-pitfalls.md) for the specific misconfigurations that silently break enforcement (kube-router `ipMasq`, k0s ConfigMap-propagation gap, kube-router service-proxy bootstrap deadlock, Calico Felix not running, Cilium identity-allocation lag).

2. **Is the deny-all baseline present in the receiving namespace?**

   Per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md) every managed namespace gets a `kube-vnet`-named deny-all baseline. If it's missing, the namespace is `disabled` (operator stays out entirely).

   ```bash
   kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet,kube-vnet/role=baseline
   ```

3. **If you want every namespace ingress-deny-by-default**, you have it already by default. To open up specific pods, make them members of a vnet (typically the system `cluster` or `namespace` vnet, or a user-defined one).

4. **Are the membership policies installed in both namespaces?**

   ```bash
   kubectl get networkpolicy -A -l kube-vnet/network=<home-ns>.<vnet>
   ```

   You should see one per namespace that has members. If a namespace is missing, members in that namespace either don't exist (the pods don't carry the join label) or are silently dropped (excluded namespace; see "My pod has the join label but isn't a member").

5. **Are the join label keys correct on both ends?**

   ```bash
   kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep kube-vnet/
   ```

   Pod-A in home namespace `platform`, vnet `payments`: `kube-vnet/net.payments=both` (or the prefixed form, also accepted in the home namespace).
   Pod-B in foreign namespace `webapp`, joining same vnet: must use the prefixed form, `kube-vnet/net.platform.payments=both`.

   If pod-B has only the bare form, the operator does not see it as a member of the `platform/payments` vnet.

---

## Egress to the public internet just started working after upgrade

Expected behavior change. As of the `ingress-isolation` rename, kube-vnet's baseline carries `policyTypes: [Ingress]` only; egress is unrestricted by the operator. The previous "deny everything except DNS + vnet members" baseline is gone (it provided narrow egress isolation that didn't actually contain the destinations that mattered, and the user-facing name `default-deny-everywhere` overpromised). See [ADR 0025](adr/0025-ingress-isolation-rename-egress-unrestricted.md) and [`security.md`](security.md).

If you need per-workload egress restriction, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` selecting your pods and listing the allowed destinations. NetworkPolicies compose additively. See the [per-workload egress allowlist recipe](recipes.md#per-workload-egress-allowlist-via-user-managed-networkpolicy).

---

## The deny-all baseline didn't appear

Per [ADR 0030](adr/0030-unified-vnet-membership-with-resolution.md), every managed namespace gets a deny-all baseline named `kube-vnet`. If it's missing, the namespace is excluded:

1. Is the namespace excluded?

   ```bash
   kubectl get ns <name> -o yaml | grep -A2 annotations:
   ```

   If `kube-vnet/disabled: "true"` is set, the operator stays out entirely — by design.

2. Is the namespace in `--disabled-namespaces`?

   ```bash
   kubectl get deploy -n kube-vnet-system kube-vnet-controller \
     -o jsonpath='{.spec.template.spec.containers[0].args}'
   ```

   The default list is `kube-system,kube-public,kube-node-lease` plus the operator's own namespace.

3. Otherwise, the baseline should be present:

   ```bash
   kubectl get netpol -n <name> kube-vnet
   ```

---

## The baseline disappeared after I deleted my vnet — bug?

No. The baseline is owned by the `NamespaceReconciler` independently of any specific vnet's lifecycle. Deleting a vnet doesn't remove the baseline. If your baseline disappeared, the most likely cause is that the namespace transitioned to `disabled` (annotation or `--disabled-namespaces` change).

---

## My VirtualNetworkBinding doesn't attach any pods

Inspect the binding's status:

```bash
kubectl get vnb -A
kubectl describe vnb -n <ns> <name>
```

Check the `Ready` condition's reason:

| Reason | Meaning | Fix |
|---|---|---|
| `PodsAttached` | Working — `attachedPods` lists the pod names. | — |
| `NoPodsMatch` | Selector matched no pods in the binding's namespace. | Verify `spec.podSelector` against the actual pod labels in the namespace. The selector is **scoped to the binding's own namespace** — there is no cross-namespace binding. |
| `VirtualNetworkNotFound` | `spec.virtualNetworkRef` does not resolve. | Check the target namespace and name. |
| `NamespaceNotAllowed` | The target vnet's `spec.allowedNamespaces` does not permit the binding's namespace. | Either add the binding's namespace to the target vnet's `allowedNamespaces`, or move the binding. |
| `NamespaceExcluded` | The binding's namespace has `kube-vnet/disabled=true` or is in `--disabled-namespaces`. | Remove the annotation, or move the binding to a managed namespace. |
| `UnknownDirection` | `spec.direction` is not one of `both`, `ingress`, `egress`, `none`. | Fix the value. |
| `InvalidSelector` | `spec.podSelector` cannot be parsed. | Fix the selector syntax. |

Once the binding is `Ready=True`, look for its generated policy:

```bash
kubectl get networkpolicy -A -l kube-vnet/binding=<binding-name>
```

The policy is named `kube-vnet-<vnet>-b-<binding>` and lives in the binding's own namespace.

---

## Binding shows Ready=False, NamespaceNotAllowed

The target vnet's `spec.allowedNamespaces` does not permit the binding's namespace. Two fixes:

```bash
# Option 1: extend the vnet's allowedNamespaces.
kubectl patch vnet -n <vnet-ns> <vnet-name> --type=merge -p '
spec:
  allowedNamespaces:
    names: [<binding-ns>]
'

# Option 2: move the binding to a permitted namespace.
```

The binding is honored only when the target vnet permits its namespace — same rule as label-driven membership.

---

## Degraded with UnknownDirection or ConflictingDirections

`UnknownDirection`: at least one pod has a join label whose value isn't `both`/`ingress`/`egress`/`none`. The pod is excluded from membership; fix the typo. Legacy `true`/`false`/empty-string aliases were dropped per ADR 0030.

```bash
kubectl describe vnet -n <ns> <name>   # the message names the offending pods
```

`ConflictingDirections`: a pod in the home namespace carries both the bare and the prefixed form of the join label, with different direction values (or one explicitly `none` and the other a member direction). The operator can't decide what the pod intends, so it excludes the pod and surfaces the conflict.

Pick one form per pod (or set both forms to the same value). See [ADR 0022](adr/0022-long-form-join-label-in-home-namespace.md).

---

## "kubectl get vnet" shows READY=False

The reason explains what to fix.

| Reason | Meaning | Fix |
|---|---|---|
| `NoMembers` | (`Ready=True` actually) — no pods are joining yet. | Add the join label to a pod. |
| `PoliciesGenerated` | (`Ready=True`) — everything's working. | Nothing to fix. |
| `InvalidName` | The vnet's name has a dot or other invalid character. | Recreate the vnet with a DNS-1123 label name (lowercase alphanumeric and hyphens, no dots). |
| `HomeNamespaceExcluded` | The vnet's home namespace is in `--disabled-namespaces` or has `kube-vnet/disabled=true`. | Move the vnet to a managed namespace, or remove the namespace from the disabled list / annotation. |
| `ApplyFailed` | The operator hit an apiserver error trying to apply a `NetworkPolicy`. | `kubectl logs deploy/kube-vnet-controller -n kube-vnet-system | grep apply` for the error detail. |
| `NameCollision` | A user-managed `NetworkPolicy` exists with the same name kube-vnet wants to use, and it doesn't carry the `kube-vnet/managed-by` label. | Rename the user policy, or move it elsewhere. |

---

## "Degraded" condition is True — what does each reason mean?

| Reason | Meaning | Fix |
|---|---|---|
| `NoIssues` | (`Degraded=False`) — clean. | — |
| `InvalidJoiners` | At least one pod carries the appropriate join label but is in a non-permitted or excluded namespace. The vnet's status message names the offending pods. | Either (a) extend `allowedNamespaces` to include the pod's namespace, (b) move the pod, or (c) remove the join label from the pod if it shouldn't be a member. The Degraded message also distinguishes whether the underlying reason was `NamespaceNotAllowed` (not in `allowedNamespaces`) or `NamespaceExcluded` (in `--disabled-namespaces` or annotated `kube-vnet/disabled=true`). |
| `UnknownDirection` | A pod's join label value is not one of `both`, `ingress`, `egress`, `none`. The pod is excluded from membership. (The legacy `true`/`false`/empty-string aliases were dropped per ADR 0030.) | Fix the value on the offending pod (named in the Degraded message). |
| `ConflictingDirections` | A home-namespace pod carries both the bare and the prefixed form of the join label with conflicting direction values. The pod is excluded from membership. | Pick one form, or set both forms to the same value. See [ADR 0022](adr/0022-long-form-join-label-in-home-namespace.md). |
| `InvalidName` | Same as Ready / `InvalidName` above. | Same fix. |
| `HomeNamespaceExcluded` | Same as Ready. | Same fix. |
| `NameCollision` | Same as Ready. | Same fix. |

The full list of constants is in `internal/controller/virtualnetwork_controller.go`; the user-facing version is [`reference/api.md`](reference/api.md).

---

## Operator logs are noisy with conflict / "object has been modified" errors

Look like:

```
"Operation cannot be fulfilled on virtualnetworks.kube-vnet.lhns.de \"X\":
the object has been modified; please apply your changes to the latest version and try again"
```

Benign. This is optimistic-concurrency in action: the reconciler tried to write the status subresource with a stale `resourceVersion`. controller-runtime retries automatically. The conflict typically means the same vnet was reconciled twice in quick succession (e.g. a pod event and a vnet-spec event arriving in the same window).

If the rate is high enough to cause real noise (more than a handful per minute), there's likely a hot vnet with very frequent pod-label churn. The reconciler still converges; the conflicts are extra work but not incorrect.

---

## Operator logs show "is being terminated" errors

Look like:

```
"networkpolicies.networking.k8s.io \"X\" is forbidden:
unable to create new content in namespace Y because it is being terminated"
```

Benign. The reconciler fired between `kubectl delete namespace Y` and the namespace finalizer completing. Kubernetes correctly refused the create. On the next reconcile, the namespace is gone and the operator does nothing in it.

If you see this *outside* of a namespace deletion (i.e. the namespace exists and is not being deleted), open an issue with the full log line.

---

## I see "PolicyRestored" Warning events — is something wrong?

Maybe. The event fires when the operator re-creates a `NetworkPolicy` that was absent immediately before its apply call — i.e. someone (or something) deleted an operator-managed policy and the operator restored it.

Inspect:

```bash
kubectl get events -A --field-selector reason=PolicyRestored --sort-by='.lastTimestamp'
```

Possible causes:

- A user manually deleted an operator-managed policy. One-off; nothing to do.
- A misbehaving controller is repeatedly deleting them. Find it and stop it.
- An attempted bypass — see [`security.md`](security.md).

If `PolicyRestored` is firing repeatedly in the same namespace (e.g. multiple times per minute), there's an active loop somewhere. The Prometheus alert `KubeVnetPolicyRestoredRepeatedly` (in [`operations.md`](operations.md)) catches this.

---

## The operator pod won't start

```bash
kubectl describe pod -n kube-vnet-system -l app.kubernetes.io/name=kube-vnet
kubectl logs -n kube-vnet-system deploy/kube-vnet-controller --previous
```

Common causes:

- **`ImagePullBackOff`**: the image isn't available where the cluster pulls from. Check `image.repository` / `image.tag` in your Helm values, and the cluster's pull-secrets / image-policy.
- **CrashLoopBackOff with "permission denied"**: the ServiceAccount RBAC didn't apply. Check `kubectl auth can-i list virtualnetworks.kube-vnet.lhns.de --as=system:serviceaccount:kube-vnet-system:kube-vnet-controller`.
- **CrashLoopBackOff with "no such CRD"**: the CRD wasn't installed. Reapply: `kubectl apply -f <release.yaml-or-equivalent>`.
- **CrashLoopBackOff with "lease create forbidden"**: the leader-election Role/RoleBinding in the operator's namespace is missing.

---

## How do I tell whether the operator is healthy?

A four-line health check:

```bash
kubectl get deploy -n kube-vnet-system kube-vnet-controller \
  -o jsonpath='Available={.status.conditions[?(@.type=="Available")].status}{"\n"}'

kubectl get lease -n kube-vnet-system kube-vnet.lhns.de \
  -o jsonpath='Holder={.spec.holderIdentity} Renewed={.spec.renewTime}{"\n"}'

kubectl get vnet -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'

# Recent operator-emitted Warning events
kubectl get events -A --field-selector type=Warning,involvedObject.kind=VirtualNetwork \
  --sort-by='.lastTimestamp' | tail -10
```

In a healthy cluster:

- `Available=True`
- `Renewed` is within the last few seconds
- Every vnet shows `True`
- No (or only old) Warning events

If you have Prometheus, also watch:

- `kube_vnet_reconciliations_total{result="error"}` — should stay at 0 or increment slowly.
- `kube_vnet_reconcile_duration_seconds` p95 — should be under a second.
- `kube_vnet_apply_errors_total` — should stay at 0.

---

## Useful inspection commands

```bash
# All vnets across all namespaces
kubectl get vnet -A

# Full state of one vnet, including conditions and members
kubectl get vnet -n <ns> <name> -o yaml

# kubectl describe shows recent Events on the vnet
kubectl describe vnet -n <ns> <name>

# All operator-managed NetworkPolicies
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet

# Just the baselines
kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet,kube-vnet/role=baseline

# Just the membership policies for a specific vnet
kubectl get networkpolicy -A -l kube-vnet/network=<home-ns>.<vnet-name>

# What's the operator running with?
kubectl get deploy -n kube-vnet-system kube-vnet-controller \
  -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'

# Operator version
kubectl get deploy -n kube-vnet-system kube-vnet-controller \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Live operator logs
kubectl logs -n kube-vnet-system deploy/kube-vnet-controller -f

# Just errors
kubectl logs -n kube-vnet-system deploy/kube-vnet-controller --tail=1000 \
  | jq -c 'select(.level=="error")'

# Recent Warning events on a vnet
kubectl get events -n <ns> --field-selector type=Warning,involvedObject.kind=VirtualNetwork

# Did a labeled pod get rejected anywhere?
kubectl get vnet -A -o json \
  | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name): \(.status.conditions[]? | select(.type=="Degraded" and .status=="True") | .message)"'
```
