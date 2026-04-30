# Troubleshooting

Symptom-first. Look up what you're seeing in the table of contents, follow the diagnostic steps.

For the full list of status-condition reasons and what each one means, see [`reference/api.md`](reference/api.md). For metrics and events, [`reference/metrics-and-events.md`](reference/metrics-and-events.md).

---

## Index

- [My pod has the join label but isn't a member](#my-pod-has-the-join-label-but-isnt-a-member)
- [Pods I expect to be isolated can talk to each other](#pods-i-expect-to-be-isolated-can-talk-to-each-other)
- [The default-deny baseline didn't appear](#the-default-deny-baseline-didnt-appear)
- [The baseline disappeared after I deleted my vnet — bug?](#the-baseline-disappeared-after-i-deleted-my-vnet--bug)
- ["kubectl get vnet" shows READY=False](#kubectl-get-vnet-shows-readyfalse)
- ["Degraded" condition is True — what does each reason mean?](#degraded-condition-is-true--what-does-each-reason-mean)
- [Operator logs are noisy with conflict / "object has been modified" errors](#operator-logs-are-noisy-with-conflict--object-has-been-modified-errors)
- [Operator logs show "is being terminated" errors](#operator-logs-show-is-being-terminated-errors)
- [I see "PolicyRestored" Warning events — is something wrong?](#i-see-policyrestored-warning-events--is-something-wrong)
- [The operator pod won't start](#the-operator-pod-wont-start)
- [How do I tell whether the operator is healthy?](#how-do-i-tell-whether-the-operator-is-healthy)
- [Useful inspection commands](#useful-inspection-commands)

---

## My pod has the join label but isn't a member

Most common case. Walk through these in order:

1. **Did you use the right label form?**
   - Pod in the VirtualNetwork's home namespace → bare: `kube-vnet/net.<vnet-name>=true`
   - Pod in *any other* namespace → prefixed: `kube-vnet/net.<home-ns>.<vnet-name>=true`

   Mixing them up is the most common cause. The pod's namespace decides which form to use, not the vnet's.

2. **Is the pod's namespace operator-excluded?**

   ```bash
   kubectl describe vnet -n <home-ns> <vnet-name> | grep -A4 Conditions:
   ```

   If `Degraded=True` with reason `InvalidJoiners` and the message names your pod's namespace, the namespace is excluded.

   Two ways a namespace can be excluded:
   - The operator-level `--excluded-namespaces` flag (defaults: `kube-system,kube-public,kube-node-lease`, plus the operator's own namespace).
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

## Pods I expect to be isolated can talk to each other

1. **Does your CNI enforce NetworkPolicy?**

   This is the #1 cause when "all the YAMLs look right but pods talk anyway." kube-vnet generates `NetworkPolicy`; your CNI is what drops packets. If the CNI doesn't enforce, the policies are decorative.

   See [`install.md`](install.md#cni-that-enforces-networkpolicy) for compatible CNIs. Quick check: install Calico/Cilium/kube-router and re-test. If isolation now works, the previous CNI didn't enforce NetworkPolicy.

2. **Is the baseline present in both namespaces?**

   Cross-namespace isolation requires the deny baseline on *both* ends. If `kube-vnet-default-deny` is missing in either namespace, that side is in default-allow mode.

   ```bash
   kubectl get networkpolicy -A -l kube-vnet/managed-by=kube-vnet,kube-vnet/role=baseline
   ```

   If a namespace lacks a baseline, either no member there has joined any vnet (so the baseline isn't installed yet — that's by design) or the namespace is opted out via annotation/exclusion.

3. **If you want every namespace deny-by-default**, turn on `--default-deny-everywhere`. See [ADR 0020](adr/0020-default-deny-unmanaged-namespaces.md) and the migration pattern in [`operations.md`](operations.md).

4. **Are the membership policies installed in both namespaces?**

   ```bash
   kubectl get networkpolicy -A -l kube-vnet/network=<home-ns>.<vnet>
   ```

   You should see one per namespace that has members. If a namespace is missing, members in that namespace either don't exist (the pods don't carry the join label) or are silently dropped (excluded namespace; see "My pod has the join label but isn't a member").

5. **Are the join label keys correct on both ends?**

   ```bash
   kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep kube-vnet/
   ```

   Pod-A in home namespace `platform`, vnet `payments`: should have `kube-vnet/net.payments=true`.
   Pod-B in foreign namespace `webapp`, joining same vnet: should have `kube-vnet/net.platform.payments=true`.

   If pod-B has the bare form (`kube-vnet/net.payments=true`), the operator does not see it as a member of the `platform/payments` vnet.

---

## The default-deny baseline didn't appear

It's installed when the *first* pod in a managed namespace becomes a member of any vnet. Check, in order:

1. Is there a member?

   ```bash
   kubectl get vnet -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {.status.members}{"\n"}{end}'
   ```

   If `members` is empty for your vnet, no one has joined yet. Without a member there's no need for a baseline (nothing to defend). See [`concepts.md`](concepts.md#the-default-deny-baseline).

2. Is the namespace operator-managed?

   ```bash
   kubectl get ns <name> -o yaml | grep -A2 annotations:
   ```

   If `kube-vnet/disabled: "true"` is set, the operator does nothing in that namespace — including not installing the baseline. By design.

3. Is the namespace in `--excluded-namespaces`?

   ```bash
   kubectl get deploy -n kube-vnet-system kube-vnet-controller \
     -o jsonpath='{.spec.template.spec.containers[0].args}'
   ```

   If your namespace appears (or `kube-system`/`kube-public`/`kube-node-lease`/the operator's own namespace), it's excluded.

4. Want a baseline in every managed namespace, including ones with no members? Turn on `--default-deny-everywhere`. See [ADR 0020](adr/0020-default-deny-unmanaged-namespaces.md).

---

## The baseline disappeared after I deleted my vnet — bug?

No, that's correct behavior. The baseline is GC'd when the last vnet member leaves a namespace (and the `--default-deny-everywhere` flag isn't on). See [`concepts.md`](concepts.md#the-default-deny-baseline) and the GC integration tests:

- `TestIntegration_BaselineGC_OnVNetDelete`
- `TestIntegration_BaselineGC_TwoVNetsKeepBaseline` (baseline stays if another vnet still has members)
- `TestIntegration_BaselineGC_ForeignNamespaceEmpties` (foreign namespace's baseline is GC'd when the foreign-side membership policy is dropped via shrunk `allowedNamespaces`)

If you don't want the GC, use `--default-deny-everywhere` so the namespace baseline is owned by the flag-driven path and survives vnet deletion.

---

## "kubectl get vnet" shows READY=False

The reason explains what to fix.

| Reason | Meaning | Fix |
|---|---|---|
| `NoMembers` | (`Ready=True` actually) — no pods are joining yet. | Add the join label to a pod. |
| `PoliciesGenerated` | (`Ready=True`) — everything's working. | Nothing to fix. |
| `InvalidName` | The vnet's name has a dot or other invalid character. | Recreate the vnet with a DNS-1123 label name (lowercase alphanumeric and hyphens, no dots). |
| `HomeNamespaceExcluded` | The vnet's home namespace is in `--excluded-namespaces` or has `kube-vnet/disabled=true`. | Move the vnet to a managed namespace, or remove the namespace from the exclusion list / annotation. |
| `ApplyFailed` | The operator hit an apiserver error trying to apply a `NetworkPolicy`. | `kubectl logs deploy/kube-vnet-controller -n kube-vnet-system | grep apply` for the error detail. |
| `NameCollision` | A user-managed `NetworkPolicy` exists with the same name kube-vnet wants to use, and it doesn't carry the `kube-vnet/managed-by` label. | Rename the user policy, or move it elsewhere. |

---

## "Degraded" condition is True — what does each reason mean?

| Reason | Meaning | Fix |
|---|---|---|
| `NoIssues` | (`Degraded=False`) — clean. | — |
| `InvalidJoiners` | At least one pod carries the appropriate join label but is in a non-permitted or excluded namespace. The vnet's status message names the offending pods. | Either (a) extend `allowedNamespaces` to include the pod's namespace, (b) move the pod, or (c) remove the join label from the pod if it shouldn't be a member. The Degraded message also distinguishes whether the underlying reason was `NamespaceNotAllowed` (not in `allowedNamespaces`) or `NamespaceExcluded` (in `--excluded-namespaces` or annotated `kube-vnet/disabled=true`). |
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
