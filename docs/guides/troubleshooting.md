# Troubleshooting

Symptom-first. Look up what you're seeing in the table of contents, follow the diagnostic steps.

For the full list of status-condition reasons and what each one means, see [`reference/api.md`](../reference/api.md). For metrics and events, [`reference/metrics-and-events.md`](../reference/metrics-and-events.md).

---

## Index

- [Pod events kube-vnet emits](#pod-events-kube-vnet-emits)
- [I can only see my own namespace](#i-can-only-see-my-own-namespace)
- [`kubectl apply` rejected my pod: "must be one of: both, ingress, egress, none"](#kubectl-apply-rejected-my-pod-must-be-one-of-both-ingress-egress-none)
- [My pod with `kube-vnet/net.X: "true"` (or `""`/`"false"`) stopped working after upgrade](#my-pod-with-kube-vnetnetx-true-or-false-stopped-working-after-upgrade)
- [My pod has the join label but isn't a member](#my-pod-has-the-join-label-but-isnt-a-member)
- [I labeled my pod and the vnet is Ready, but external pods can still reach it](#i-labeled-my-pod-and-the-vnet-is-ready-but-external-pods-can-still-reach-it)
- [A Job or one-shot pod fails to connect on startup, but succeeds on retry](#a-job-or-one-shot-pod-fails-to-connect-on-startup-but-succeeds-on-retry)
- [Pods I expect to be isolated can talk to each other](#pods-i-expect-to-be-isolated-can-talk-to-each-other)
- [Admission webhook fails with `context deadline exceeded`](#admission-webhook-fails-with-context-deadline-exceeded)
- [Pod creation fails in kube-vnet's own webhook (`pods.systemlabels.kube-vnet.lhns.de`)](#pod-creation-fails-in-kube-vnets-own-webhook-podssystemlabelskube-vnetlhnsde)
- [CNI pitfalls that silently break enforcement (separate page)](cni-pitfalls.md)
- [Egress to the public internet just started working after upgrade](#egress-to-the-public-internet-just-started-working-after-upgrade)
- [The deny-all baseline didn't appear](#the-deny-all-baseline-didnt-appear)
- [The baseline disappeared after I deleted my vnet — bug?](#the-baseline-disappeared-after-i-deleted-my-vnet--bug)
- [A namespace is stuck in `Terminating`](#a-namespace-is-stuck-in-terminating)
- [My VirtualNetworkBinding doesn't attach any pods](#my-virtualnetworkbinding-doesnt-attach-any-pods)
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

A `VirtualNetworkNotJoinable` Warning fires when a membership can't be honored — on the Pod for a `kube-vnet/net.*` label, on the `VirtualNetworkBinding` or baseline for a ref declared there. It surfaces in `kubectl describe` and via `kubectl get events --field-selector reason=VirtualNetworkNotJoinable -A`. The message tells you which of the cases below applies. A label with an unrecognized direction value gets `InvalidDirection` instead, and rules that disagree about a vnet get `ResolutionConflict` or `OverrideRejected` ([all reasons](../reference/metrics-and-events.md#kubernetes-events)). See [ADR 0027](../adr/0027-pod-scoped-join-label-events.md) (retirement amendment) and [ADR 0043](../adr/0043-virtualnetworkref-namespace-inferred-or-honored.md).

> Pods in a `kube-vnet/disabled=true` (or `--disabled-namespaces`) namespace do not get this event. A pod there that carries a `kube-vnet/net.*` label gets one `NamespaceExcluded` Warning instead, saying the label has no effect; pods without one get nothing.
>
> Events are best-effort and expire after an hour. For pods in namespaces the vnet admits, its `Degraded`/`InvalidJoiners` condition keeps a durable record; a pod that asked to join from elsewhere has only its Event, re-emitted whenever the pod is re-resolved.

### Bare label, no local vnet

**Symptom.** The pod has `kube-vnet/net.<X>` (bare form) and isn't a member. `kubectl describe pod` shows:

```
Events:
  Type     Reason                       Age   From                 Message
  ----     ------                       ----  ----                 -------
  Warning  VirtualNetworkNotJoinable    10s   kube-vnet-resolution  cannot join VirtualNetwork <this-pod-ns>/X (from pod label kube-vnet/net.X): VirtualNetwork <this-pod-ns>/X does not exist. hint: the bare form "kube-vnet/net.X" is only honored in the vnet's home namespace; to join a vnet hosted in another namespace use the prefixed form "kube-vnet/net.<homeNS>.X".
```

**Cause.** No `VirtualNetwork` of name `<X>` exists in the pod's *own* namespace. The bare form only resolves against the pod's namespace.

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

### Prefixed label, vnet doesn't exist

**Symptom.** The pod has `kube-vnet/net.<homeNS>.<X>` and isn't a member. The `VirtualNetworkNotJoinable` message reads `… VirtualNetwork <homeNS>/X does not exist.`

**Cause.** The vnet `<homeNS>/<X>` doesn't exist — either typo'd home-namespace, typo'd vnet name, or the vnet hasn't been created yet.

**Fix.** Verify the vnet exists at the named home:

```bash
kubectl get vnet -n <homeNS> <X>
```

Either correct the label key, or apply the missing `VirtualNetwork` manifest.

### Prefixed label, namespace not allowed

**Symptom.** The pod has `kube-vnet/net.<homeNS>.<X>`, the vnet `<homeNS>/<X>` exists, and the pod still isn't a member. The `VirtualNetworkNotJoinable` message reads `… VirtualNetwork <homeNS>/X does not permit namespace "<this-pod-ns>"; its owner can add it to spec.allowedNamespaces.`

**Cause.** The vnet's `spec.allowedNamespaces` does not permit the pod's namespace. Only the pod owner is told: the vnet's `Degraded` status counts only pods in namespaces it admits, so a tenant can't mark someone else's vnet degraded by labelling a pod.

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

## `kubectl apply` rejected my pod: "must be one of: both, ingress, egress, none"

```
$ kubectl apply -f mypod.yaml
The pods "my-pod" is invalid: ValidatingAdmissionPolicy "kube-vnet-join-label-direction"
denied request: kube-vnet join label values must be one of: both, ingress, egress, none.
The legacy true/false/empty values are no longer accepted.
```

**Cause.** Kubernetes ≥ 1.30 with the kube-vnet chart installed runs a `ValidatingAdmissionPolicy` that rejects Pod create/update when any `kube-vnet/net.*` label has an unrecognized value (typo like `bothh`, or an arbitrary string). See [ADR 0027](../adr/0027-pod-scoped-join-label-events.md).

**Fix.** Set the value to one of the recognized direction strings:

```yaml
labels:
  kube-vnet/net.payments: both        # bidirectional, the usual choice
  # or `ingress`, `egress`, `none`
```

The legacy `true`/`false`/empty-string aliases are no longer accepted (dropped per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md); see the [ADR 0021 2026-05-05 addendum](../adr/0021-direction-modes-on-join-labels.md#addendum-2026-05-05--legacy-truefalseempty-aliases-dropped)). On clusters older than 1.30 the chart doesn't install the VAP (it checks the Kubernetes version). The same typo is then admitted but ignored at reconcile time: the pod gets an `InvalidDirection` event and the vnet `Degraded=True, reason=InvalidJoiners` with per-pod reason `InvalidDirection` — see [Degraded reasons](#degraded-condition-is-true--what-does-each-reason-mean).

---

## My pod with `kube-vnet/net.X: "true"` (or `""`/`"false"`) stopped working after upgrade

**Symptom.** A pod manifest that used `kube-vnet/net.<vnet>: "true"` (or `""`, or `"false"`) is now rejected at admission, or excluded from membership on older clusters.

**Cause.** Breaking change. The legacy aliases `true`, `false`, and the empty-string value were dropped per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md). The valid direction values are now exactly `both`, `ingress`, `egress`, `none`.

**Fix.** Set an explicit value:

```yaml
labels:
  kube-vnet/net.payments: both     # was: "true" or ""
  # or `none`               (was: "false" or "")
```

---

## I can only see my own namespace

Many users can read only their own namespace. From there you can't read a vnet hosted in another namespace, the `cluster` vnet (it lives in the operator's namespace), the Namespace object, or the operator's logs. Everything kube-vnet reports about your workloads lands in your namespace instead:

1. **The pod.** `kubectl describe pod <pod>` lists its Events: `VirtualNetworkNotJoinable`, `InvalidDirection`, `ResolutionConflict`, `OverrideRejected`, `NamespaceExcluded`, `NetworkWaitSkipped` ([what each means](../reference/metrics-and-events.md#kubernetes-events)). Its labels say whether it is a member. `kube-vnet.system/net.<home-ns>.<vnet>=<direction>` means it is (bare `kube-vnet.system/net.cluster` for the `cluster` vnet). No such label while `kube-vnet.system/resolved-generation` is set means resolution decided it is not, and the Events say why.

   ```bash
   kubectl get pod <pod> -o jsonpath='{.metadata.labels}{"\n"}{.metadata.annotations}{"\n"}'
   ```

2. **Your namespace's Warnings.** A membership policy that could not be applied here, a baseline that could not be applied (your namespace then has no default-deny), a restored policy, and Service problems all show up here:

   ```bash
   kubectl get events -n <ns> --field-selector type=Warning --sort-by=.lastTimestamp
   kubectl get events -n <ns> --field-selector reason=ApplyFailed
   ```

3. **Your bindings.** `kubectl describe vnb -n <ns> <binding>` shows the binding's `Ready` condition and a `VirtualNetworkNotJoinable` Event if its vnet can't be joined.

4. **The NetworkPolicies in your namespace.** `kube-vnet.base` is the baseline. There is one `kube-vnet.mem.<home-ns>.<vnet>-<hash>` per vnet with members here, and `kube-vnet.ext.*` for auto-allow:

   ```bash
   kubectl get netpol -n <ns> -l kube-vnet.system/managed-by=kube-vnet
   kubectl get netpol -n <ns> -l kube-vnet.system/network=<home-ns>.<vnet> -o yaml
   ```

   A stamped pod with no membership policy here usually means an `ApplyFailed` Event from step 2.

5. **Still stuck?** Send the vnet's owner or a cluster admin the pod, namespace, vnet `<home-ns>/<name>` and the Events above. They can read the vnet's conditions and the operator's logs.

---

## My pod has the join label but isn't a member

Most common case. Walk through these in order. Steps 2, 3 and 5 read the vnet or the Namespace; if you can't, use [I can only see my own namespace](#i-can-only-see-my-own-namespace).

1. **Did you use the right label form?**
   - Pod in the VirtualNetwork's home namespace → either form works: `kube-vnet/net.<vnet-name>=both` or `kube-vnet/net.<home-ns>.<vnet-name>=both`.
   - Pod in *any other* namespace → prefixed only: `kube-vnet/net.<home-ns>.<vnet-name>=both`.

   The pod's namespace decides which forms are valid, not the vnet's.

   **Direction value.** The value must be `both`, `ingress`, `egress`, or `none`. An unknown value (e.g. a typo `"bothh"`) is rejected at admission, or — without the VAP — ignored with an `InvalidDirection` pod event.

   **Another rule disagrees.** If a binding or baseline also names the vnet, the directions combine: same-tier rules intersect (`ingress` and `egress` give no membership), and a baseline's bare value can't be overridden. `kubectl describe pod` shows a `ResolutionConflict` or `OverrideRejected` Warning naming the rules involved.

2. **Is the pod's namespace operator-excluded?**

   ```bash
   kubectl describe vnet -n <home-ns> <vnet-name> | grep -A4 Conditions:
   ```

   If `Degraded=True` with reason `InvalidJoiners` and the message names your pod's namespace, the namespace is excluded. Without access to the vnet: a pod with a join label there has a `NamespaceExcluded` Warning (`kubectl describe pod`).

   Two ways a namespace can be excluded:
   - The operator-level `--disabled-namespaces` flag (default `kube-system`, plus the operator's own namespace).
   - The per-namespace annotation: `kubectl get ns <name> -o jsonpath='{.metadata.annotations.kube-vnet/disabled}'` — if it reads `true`, that's why.

3. **Is the pod's namespace permitted by `allowedNamespaces`?**

   ```bash
   kubectl get vnet -n <home-ns> <vnet-name> -o yaml | yq .spec.allowedNamespaces
   ```

   If `allowedNamespaces` is unset, only the home namespace can join. If it's `names: [...]`, your namespace must be in that list (exact match — no globs). If it's `selector: {...}`, your namespace's labels must match; a malformed selector matches no namespaces.

   A pod in a non-permitted namespace gets a `VirtualNetworkNotJoinable` Warning saying so; it does not appear on the vnet.

4. **Is the operator alive?**

   ```bash
   kubectl get deploy -n kube-vnet-system kube-vnet
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

The deny-all baseline selects every pod; there's no per-pod exemption (the `--elide-baseline-for` flag was removed in [ADR 0035](../adr/0035-removal-of-elide-baseline-for.md)). What makes a pod "open" is membership in the cluster system vnet: if your pod is on `cluster=both`/`ingress` (e.g. because `operator.clusterBaseline.ingressIsolationLevel=cluster` seeds `cluster=default-both`), the cluster-vnet membership policy adds allow-from-cluster-peer rules that override the baseline's deny-all via NetworkPolicy union semantics. Under that preset every pod is on `cluster`, so cluster peers ≈ everyone.

To enforce stricter ingress on a specific pod:

- Don't make the pod a `cluster=both` member. Either remove the operator default or set `kube-vnet/net.cluster=none` on the pod.
- The deny-all baseline will then apply, and only the user-vnet membership policies grant ingress.

For the full design, see the [deny-all baseline section in `concepts.md`](../getting-started/concepts.md#the-deny-all-baseline) and [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md).

## A Job or one-shot pod fails to connect on startup, but succeeds on retry

A migration or backup Job fails immediately, and the same Job with a `sleep` in front of it works.
The sleep is a guess at a duration you do not control. Two delays add up before a new pod's traffic
is allowed:

1. **kube-vnet's.** Membership policies select pods by the `kube-vnet.system/net.*` label the
   operator stamps *after* the apiserver persists the pod; until then the deny-all baseline
   applies. Usually under a second, longer under load, during an operator restart, or with a slow
   apiserver. The admission webhook (`webhook.enabled=true`,
   [ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md)) removes it by stamping at
   admission.
2. **The CNI's.** The CNI then programs the pod's IP into its rules. On kube-router this is a full
   iptables rewrite per pod event, 2–4 s end to end on one production cluster, growing with the
   number of NetworkPolicies. The network wait below covers it.

**The fix on kube-router: enable the webhook and the network wait** (`webhook.enabled=true`,
`webhook.networkWait.enabled=true`), then annotate the pod template:

```yaml
metadata:
  annotations:
    kube-vnet/network-max-wait: "30s"
```

An injected init container holds the app until every node's beacon accepts the pod, and never
longer than the maximum ([ADR 0045](../adr/0045-network-wait-for-opted-in-pods.md), which also has
the per-CNI measurements). Its log says how long each node took:

```bash
kubectl logs -n <ns> <pod> -c kube-vnet-network-wait
```

`max wait ... reached; starting anyway` lists the beacon IPs that never accepted;
`kubectl get pods -n kube-vnet-system -o wide -l app.kubernetes.io/component=network-beacon` maps
them to nodes. Look for a node whose CNI is stuck, or an egress policy or mesh that blocks the probe
to the `<release>-network-beacon` pods. If the beacon Service never resolved, check that the beacon
DaemonSet is running and that the pod can reach cluster DNS. The pod gets a `NetworkWaitSkipped`
Event if it started without the wait. On CNIs other than kube-router the wait is a strong hint, not
a guarantee; if the first connection still fails there, use the checks below.

**First establish what a denial looks like on your CNI**, or a refusal will read like "nothing is
listening" rather than "policy denied".

```bash
# A pod identical to yours, minus the join label. It should be denied for as
# long as it runs.
kubectl run denial-control --image=curlimages/curl -n <ns> -it --rm --restart=Never -- \
  sh -c 'for i in $(seq 1 20); do curl -s -o /dev/null -m 2 -w "%{http_code} " \
    http://<svc>.<target-ns>.svc.cluster.local/ ; echo; sleep 1; done'
```

- Consistently **`Connection refused`** (immediate) — that is your denial signature. kube-router
  with iptables behaves this way.
- Consistently **hangs / times out** — your CNI drops rather than rejects.

Now re-run your real probe and compare. A result matching the control means policy, not the
application.

**Then separate "policy not programmed" from "endpoints not ready"** by retrying against the pod
IP, which bypasses Service endpoint selection entirely:

```bash
kubectl get pod -n <target-ns> <pod> -o jsonpath='{.status.podIP}'
```

- Pod IP works, ClusterIP does not → endpoint readiness, not kube-vnet.
- **Both** fail → the allow rule is not programmed yet. That is this window.

**Confirm from the pod itself** — the stamp is either there or it is not:

```bash
kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep kube-vnet.system
kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.annotations.kube-vnet\.system/resolved-by}'
```

`resolved-by` is `admission` when the webhook stamped it and `controller` when the reconciler did.
Seeing `controller` on a cluster where you enabled the webhook means the webhook was unreachable
for that pod and it fell back — which is safe, but it is also the window reappearing.

**If you cannot enable the network wait**, gate on the real condition rather than on a duration. An
initContainer shares the pod's network namespace and IP, so it tests exactly what the main
container needs — can *this pod* reach *that target*:

```yaml
initContainers:
  - name: wait-for-target
    image: curlimages/curl:8.10.1
    command: ["sh", "-c"]
    args:
      - |
        until curl -sf -m 2 http://<svc>.<target-ns>.svc.cluster.local/health; do
          echo "target not reachable yet"; sleep 1
        done
```

This covers the policy window and target readiness together, and it stops being a guess.

## Pods I expect to be isolated can talk to each other

1. **Does your CNI enforce NetworkPolicy?**

   This is the #1 cause when "all the YAMLs look right but pods talk anyway." kube-vnet generates `NetworkPolicy`; your CNI is what drops packets. If the CNI doesn't enforce, the policies are decorative.

   See [`install.md`](../getting-started/install.md#cni-that-enforces-networkpolicy) for compatible CNIs. Quick check: install Calico/Cilium/kube-router and re-test. If isolation now works, the previous CNI didn't enforce NetworkPolicy.

   If your CNI *claims* to enforce NetworkPolicy and isolation still doesn't work, see [`cni-pitfalls.md`](cni-pitfalls.md) for the specific misconfigurations that silently break enforcement (kube-router `ipMasq`, k0s ConfigMap-propagation gap, kube-router service-proxy bootstrap deadlock, Calico Felix not running, Cilium identity-allocation lag).

2. **Is the deny-all baseline present in the receiving namespace?**

   Per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md) every managed namespace gets a deny-all baseline named `kube-vnet.base`. If it's missing, the namespace is `disabled` (operator stays out entirely).

   ```bash
   kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet,kube-vnet.system/role=baseline
   ```

3. **If you want every namespace ingress-deny-by-default**, you have it already by default. To open up specific pods, make them members of a vnet (typically the system `cluster` or `namespace` vnet, or a user-defined one).

4. **Are the membership policies installed in both namespaces?**

   ```bash
   kubectl get networkpolicy -A -l kube-vnet.system/network=<home-ns>.<vnet>
   ```

   You should see one per namespace that has members. If a namespace is missing, members in that namespace either don't exist (the pods don't carry the join label) or are silently dropped (excluded namespace; see "My pod has the join label but isn't a member").

5. **Are the join label keys correct on both ends?**

   ```bash
   kubectl get pod -n <ns> <pod> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep kube-vnet/
   ```

   Pod-A in home namespace `platform`, vnet `payments`: `kube-vnet/net.payments=both` (or the prefixed form, also accepted in the home namespace).
   Pod-B in foreign namespace `webapp`, joining same vnet: must use the prefixed form, `kube-vnet/net.platform.payments=both`.

   If pod-B has only the bare form, the operator does not see it as a member of the `platform/payments` vnet.

6. **Cross-NS connections opened *before* the policy tightened — are they riding conntrack amnesty?**

   NetworkPolicy is evaluated only at SYN-time. Existing ESTABLISHED connections in the kernel's conntrack table are immune to new policy decisions (Linux's default ESTABLISHED timeout is ~5 days). Reverse proxies with backend connection pools (traefik, nginx-ingress, envoy) commonly hold long-lived HTTP/2 streams to backend pods — every request over that stream bypasses any policy added after the connection was opened.

   Verify with a fresh pod-to-pod probe (no cached connections):

   ```bash
   kubectl run probe --image=curlimages/curl -n <source-ns> -it --rm --restart=Never -- \
     curl -sv -m 5 http://<svc>.<target-ns>.svc.cluster.local/
   ```

   Expected under correct isolation: a denial. **What a denial looks like is CNI-specific** —
   an immediate `Connection refused` on kube-router/iptables, a hang on CNIs that drop. Establish
   yours with the unlabelled control pod described in
   [A Job or one-shot pod fails to connect on startup](#a-job-or-one-shot-pod-fails-to-connect-on-startup-but-succeeds-on-retry)
   before reading anything into the result. If the probe is denied but real traffic from the source workload (e.g. traefik) still flows, the workload is holding a stale ESTABLISHED entry. Find it:

   ```bash
   SRC_POD=$(kubectl get pods -n <source-ns> -l <selector> -o jsonpath='{.items[0].metadata.name}')
   TARGET_IP=$(kubectl get pods -n <target-ns> -l <selector> -o jsonpath='{.items[0].status.podIP}')
   kubectl exec -n <source-ns> "$SRC_POD" -- netstat -an 2>/dev/null | grep "$TARGET_IP"
   # Look for ESTABLISHED tcp ... → $TARGET_IP:<port>
   ```

   For DaemonSets (ingress controllers etc.), check every pod — load-balancing can route traffic through any of them, so one cached connection on any pod is enough to keep traffic flowing intermittently.

   **Fix**: force fresh connections by restarting the source workload:

   ```bash
   kubectl rollout restart deploy/<source-workload> -n <source-ns>
   # or
   kubectl rollout restart daemonset/<source-daemon> -n <source-ns>
   ```

   After the rollout completes, every cached connection is gone. New connection attempts get evaluated by the now-tightened policy and are denied. See [FAQ § "I tightened isolation but existing cross-namespace connections still work. Why?"](../faq.md#i-tightened-isolation-but-existing-cross-namespace-connections-still-work-why) for the background on why this is a Linux-conntrack reality, not a kube-vnet bug.

---

## Admission webhook fails with `context deadline exceeded`

Symptom — `kubectl apply` of a CR (most often a `Certificate`, `Issuer`, `ClusterPolicy`, or any custom resource a webhook validates) hangs ~30s then fails with:

```
Error from server (InternalError): error when creating "cert.yaml":
  Internal error occurred: failed calling webhook "webhook.cert-manager.io":
  Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s":
  context deadline exceeded
```

Background: the kube-apiserver dials the webhook backend Service directly. The apiserver isn't a pod, so its source IP (control-plane node IP for kubeadm / k0s / k3s; managed-control-plane IP for GKE / EKS / AKS) doesn't match any `namespaceSelector` or `podSelector`. The webhook NS's baseline + membership policies reject the connection. Admission times out.

Since v0.4.0 kube-vnet auto-emits an allow on the webhook port whenever a `ValidatingWebhookConfiguration`, `MutatingWebhookConfiguration`, `APIService`, or CRD conversion webhook references a Service in a managed namespace. Model + opt-outs: [the auto-allow guide](auto-allow.md); design: [ADR 0041](../adr/0041-auto-allow-apiserver-reachable-services.md).

**Diagnostic steps in order**:

1. Find the webhook config and the Service it points at:

   ```bash
   kubectl get validatingwebhookconfigurations -o jsonpath='{range .items[*]}{.metadata.name}: {.webhooks[*].clientConfig.service.namespace}/{.webhooks[*].clientConfig.service.name}{"\n"}{end}'
   kubectl get mutatingwebhookconfigurations -o jsonpath='{range .items[*]}{.metadata.name}: {.webhooks[*].clientConfig.service.namespace}/{.webhooks[*].clientConfig.service.name}{"\n"}{end}'
   ```

2. Check whether the auto-allow policy is in place for that Service:

   ```bash
   kubectl get netpol -n cert-manager -l kube-vnet.system/source-kind=apiserver
   # Expected: kube-vnet.ext.apiserver.cert-manager-webhook-<8hex>
   ```

3. If the policy is missing, check the most common causes:

   - **The webhook backend NS is `disabledNamespaces`** or has `kube-vnet/disabled=true`. The operator skips disabled namespaces entirely. Remove the NS from the disabled list, or move the webhook Service elsewhere.

     ```bash
     kubectl get ns cert-manager -o jsonpath='{.metadata.annotations.kube-vnet/disabled}{"\n"}'
     ```

   - **Service or NS opted out** via `kube-vnet/external-allow=false`:

     ```bash
     kubectl get svc -n cert-manager cert-manager-webhook -o jsonpath='{.metadata.annotations.kube-vnet/external-allow}{"\n"}'
     kubectl get ns cert-manager -o jsonpath='{.metadata.annotations.kube-vnet/external-allow}{"\n"}'
     ```

   - **The webhook uses `clientConfig.url`** (out-of-cluster endpoint). ADR 0041 doesn't auto-emit for URL-only webhooks; they're not in-cluster sources. If the webhook IS in-cluster but configured via URL, switch the config to `clientConfig.service`.

4. If the policy exists but admission still times out, the most likely cause is a too-narrow `operator.apiserverSourceCIDR`. The default is `0.0.0.0/0`. If you've set it to a narrower range that doesn't include the apiserver's source IP, the policy is too tight. Inspect:

   ```bash
   kubectl get netpol -n cert-manager kube-vnet.ext.apiserver.cert-manager-webhook-... -o jsonpath='{.spec.ingress[*].from[*].ipBlock.cidr}{"\n"}'
   ```

   Find your apiserver's source IP (the control-plane node IPs for kubeadm/k0s/k3s):

   ```bash
   kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'
   ```

   Either widen `apiserverSourceCIDR` to cover those IPs, or set it back to the default `0.0.0.0/0`.

5. As a quick unblocker (loses pod-to-pod isolation in the NS):

   ```bash
   kubectl annotate ns cert-manager kube-vnet/disabled=true
   ```

   This turns kube-vnet off for the cert-manager NS entirely. Use only if ADR 0041's auto-allow doesn't fit your scenario for some reason; the targeted fix is preferred.

---

## Pod creation fails in kube-vnet's own webhook (`pods.systemlabels.kube-vnet.lhns.de`)

Only with `webhook.enabled=true` ([ADR 0034](../adr/0034-admission-webhook-for-pod-resolution.md)). Two different errors:

**`failed calling webhook "pods.systemlabels.kube-vnet.lhns.de"`** (connection refused, timeout, or a TLS error). The validating webhook is `failurePolicy: Fail`, so while no operator replica serves it, pod creation and update in managed namespaces is rejected. A replica whose webhook server is not serving (serving cert missing, port taken) reports not Ready and drops out of the `kube-vnet-webhook` endpoints, so no Ready replica usually means no endpoints. Check the operator:

```bash
kubectl get deploy -n kube-vnet-system kube-vnet
kubectl get endpointslices -n kube-vnet-system -l kubernetes.io/service-name=kube-vnet-webhook
kubectl logs -n kube-vnet-system deploy/kube-vnet | grep -i webhook
```

The operator's own namespace, `kube-system`, `kube-public`, `kube-node-lease` and `operator.disabledNamespaces` are exempt, so the operator can always restart. Run at least two replicas. To unblock the cluster while you fix it, delete the two webhook configurations; `helm upgrade` recreates them:

```bash
kubectl delete mutatingwebhookconfiguration,validatingwebhookconfiguration kube-vnet-pod-resolution
```

**`admission webhook "pods.systemlabels.kube-vnet.lhns.de" denied the request: labels under kube-vnet.system/ are managed by the kube-vnet operator ...`**. The request sets, changes or removes a `kube-vnet.system/*` label to something resolution does not produce, e.g. a manifest copied from `kubectl get pod -o yaml` with stale stamps. Remove the `kube-vnet.system/*` labels from the manifest and declare membership with a `kube-vnet/net.*` label instead.

---

## Egress to the public internet just started working after upgrade

Expected behavior change. As of the `ingress-isolation` rename, kube-vnet's baseline carries `policyTypes: [Ingress]` only; egress is unrestricted by the operator. The previous "deny everything except DNS + vnet members" baseline is gone (it provided narrow egress isolation that didn't actually contain the destinations that mattered, and the user-facing name `default-deny-everywhere` overpromised). See [ADR 0025](../adr/0025-ingress-isolation-rename-egress-unrestricted.md) and [`security.md`](../security/security.md).

If you need per-workload egress restriction, write a user-managed `NetworkPolicy` with `policyTypes: [Egress]` selecting your pods and listing the allowed destinations. NetworkPolicies compose additively. See the [per-workload egress allowlist recipe](recipes.md#per-workload-egress-allowlist-via-user-managed-networkpolicy).

---

## The deny-all baseline didn't appear

Per [ADR 0030](../adr/0030-unified-vnet-membership-with-resolution.md), every managed namespace gets a deny-all baseline named `kube-vnet.base`. If it's missing, the namespace is excluded:

1. Is the namespace excluded?

   ```bash
   kubectl get ns <name> -o yaml | grep -A2 annotations:
   ```

   If `kube-vnet/disabled: "true"` is set, the operator stays out entirely — by design.

2. Is the namespace in `--disabled-namespaces`?

   ```bash
   kubectl get deploy -n kube-vnet-system kube-vnet \
     -o jsonpath='{.spec.template.spec.containers[0].args}'
   ```

   The default list is `kube-system` plus the operator's own namespace.

3. Otherwise, the baseline should be present:

   ```bash
   kubectl get netpol -n <name> kube-vnet.base
   ```

---

## The baseline disappeared after I deleted my vnet — bug?

No. The baseline is owned by the `NamespaceReconciler` independently of any specific vnet's lifecycle. Deleting a vnet doesn't remove the baseline. If your baseline disappeared, the most likely cause is that the namespace transitioned to `disabled` (annotation or `--disabled-namespaces` change).

---

## A namespace is stuck in `Terminating`

**Symptom.** `kubectl delete namespace <ns>` never completes; `kubectl get ns <ns>` shows `Terminating` indefinitely. The namespace ran kube-vnet (it was managed).

**Cause (fixed in v0.4.1).** The operator creates a `namespace` system VirtualNetwork in every managed namespace. Before v0.4.1, the `…-system-vnet-protected` ValidatingAdmissionPolicy guarded `DELETE` — so when Kubernetes' namespace controller tried to cascade-delete that vnet during teardown, admission denied it (the namespace controller isn't the operator's ServiceAccount), and the namespace could never finish terminating. Confirm the leftover vnet:

```bash
kubectl get vnet -n <ns> namespace   # still present on a Terminating namespace
```

**Fix.** Upgrade to v0.4.1 or later, where the VAP guards `CREATE`/`UPDATE` only — namespace teardown then completes normally, and user-initiated deletes of a system vnet are still recovered by drift-correction. (NetworkPolicies were never affected: no VAP guards their `DELETE`.)

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
| `PodsAttached` | Working — `attachedPods` lists the member pods. If the message says "N of M selected pod(s)", the rest are selected but not members. | Check the other pods' events and `kube-vnet.system/net.*` labels. |
| `NoPodsAttached` | `Ready=True`; the selector matches pods but none is a member (a baseline or pod-label conflict, direction `none`, or not stamped yet). | Check the pods' events and `kube-vnet.system/net.*` labels. |
| `HomeNamespaceExcluded` | The target vnet's home namespace is disabled or excluded, so the vnet is not served. | Re-enable the home namespace, or bind to a vnet in a managed namespace. |
| `VirtualNetworkTerminating` | The target vnet is being deleted. | Recreate the vnet or point the binding elsewhere. |
| `NoPodsMatch` | `Ready=True`, but the selector matches no pods in the binding's namespace. | Check `spec.podSelector` against the pod labels. Bindings select only in their own namespace. |
| `VirtualNetworkNotJoinable` | The target vnet does not exist, or its `spec.allowedNamespaces` does not permit the binding's namespace; the message says which. | "does not exist": check the target namespace and name. "does not permit": ask the vnet's owner to add the binding's namespace to `allowedNamespaces`, or move the binding. |
| `NamespaceExcluded` | The binding's namespace has `kube-vnet/disabled=true` or is in `--disabled-namespaces`. | Remove the annotation, or move the binding to a managed namespace. |
| `InvalidDirection` | `spec.direction` is not one of `both`, `ingress`, `egress`, `none`. | Fix the value. |
| `InvalidSelector` | `spec.podSelector` cannot be parsed. | Fix the selector syntax. |

Once the binding is `Ready=True`, the resolution controller stamps the canonical FQ system label `kube-vnet.system/net.<homeNS>.<vnet>` on each selected pod (per [ADR 0033](../adr/0033-canonical-fq-system-labels.md)). The pods are then covered by the regular per-`(vnet, namespace)` membership policy — no per-binding policy is emitted. To inspect:

```bash
kubectl get networkpolicy -A -l kube-vnet.system/network=<homeNS>.<vnet>
```

The membership policy is named `kube-vnet.mem.<homeNS>.<vnet>-<8hex>` and lives in each member-bearing namespace.

---

## "kubectl get vnet" shows READY=False

The reason explains what to fix.

| Reason | Meaning | Fix |
|---|---|---|
| `NoMembers` | (`Ready=True` actually) — no pods are joining yet. | Add the join label to a pod. |
| `PoliciesGenerated` | (`Ready=True`) — everything's working. | Nothing to fix. |
| `InvalidName` | The vnet's name has a dot or other invalid character. | Recreate the vnet with a DNS-1123 label name (lowercase alphanumeric and hyphens, no dots). |
| `HomeNamespaceExcluded` | The vnet's home namespace is in `--disabled-namespaces` or has `kube-vnet/disabled=true`. | Move the vnet to a managed namespace, or remove the namespace from the disabled list / annotation. |
| `ApplyFailed` | The apiserver rejected one or more membership `NetworkPolicy` applies (typically a ResourceQuota or an admission policy in the member namespace). | The condition message has the first three errors. Each failed member namespace also has an `ApplyFailed` Event on the policy: `kubectl get events -n <member-ns> --field-selector reason=ApplyFailed`. Fix the quota or policy there; the operator retries. |

---

## "Degraded" condition is True — what does each reason mean?

| Reason | Meaning | Fix |
|---|---|---|
| `NoIssues` | (`Degraded=False`) — clean. | — |
| `InvalidJoiners` | A pod in a namespace this vnet admits has a join label for it that can't be honored. The message lists up to three as `<ns>/<pod>:<reason>` (`InvalidDirection` or `NamespaceExcluded`). Pods in namespaces the vnet doesn't admit get a `VirtualNetworkNotJoinable` Event instead. | Fix the value, or remove the join label if the pod shouldn't be a member. |
| `InvalidName` | Same as Ready / `InvalidName` above. | Same fix. |
| `HomeNamespaceExcluded` | Same as Ready. | Same fix. |

**Conflicting directions** for the same vnet from different sources (a binding says `both`, the pod label `egress`; or two bindings disagree) are intersected fail-closed ([ADR 0031](../adr/0031-baseline-tier-resolution.md)) — here, `egress`. They don't show on the vnet: the pod gets a `ResolutionConflict` (or, for an override of a pinned baseline value, `OverrideRejected`) Warning Event naming the sources and the result. Bare-vs-prefixed labels on the same pod canonicalize to one key ([ADR 0033](../adr/0033-canonical-fq-system-labels.md)).

Reason definitions: [`reference/api.md`](../reference/api.md#degraded-condition).

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

Benign. A reconcile fired between `kubectl delete namespace Y` and the namespace finalizer completing, and Kubernetes correctly refused the create. Every reconciler that creates namespaced objects (baseline, system vnet, VirtualNetwork membership policies, host-port, external-allow, apiserver-reachable) skips a namespace that carries a `DeletionTimestamp`, so this is rare, but a reconcile that read the namespace just before it started terminating can still produce one such line.

If you see this *outside* of a namespace deletion (i.e. the namespace exists and is not being deleted), open an issue with the full log line.

---

## I see "PolicyRestored" Warning events — is something wrong?

Maybe. The event fires when the operator re-creates a `NetworkPolicy` it had applied before and that was absent immediately before its apply call — someone (or something) deleted it and the operator restored it. It is emitted on the restored policy, in its namespace, for every kind (membership, baseline, auto-allow); a membership restore also shows on the vnet.

Inspect:

```bash
kubectl get events -A --field-selector reason=PolicyRestored --sort-by='.lastTimestamp'
```

Possible causes:

- A user manually deleted an operator-managed policy. One-off; nothing to do.
- A misbehaving controller is repeatedly deleting them. Find it and stop it.
- An attempted bypass — see [`security.md`](../security/security.md).

If `PolicyRestored` is firing repeatedly in the same namespace (e.g. multiple times per minute), there's an active loop somewhere. `PolicyRestored` is an Event, not a metric, so alerting on it needs Events forwarded ([metrics-and-events](../reference/metrics-and-events.md#forward-events-to-your-aggregator)).

---

## The operator pod won't start

```bash
kubectl describe pod -n kube-vnet-system -l app.kubernetes.io/name=kube-vnet
kubectl logs -n kube-vnet-system deploy/kube-vnet --previous
```

Common causes:

- **`ImagePullBackOff`**: the image isn't available where the cluster pulls from. Check `image.repository` / `image.tag` in your Helm values, and the cluster's pull-secrets / image-policy.
- **CrashLoopBackOff with "permission denied"**: the ServiceAccount RBAC didn't apply. Check `kubectl auth can-i list virtualnetworks.kube-vnet.lhns.de --as=system:serviceaccount:kube-vnet-system:kube-vnet`.
- **CrashLoopBackOff with "no such CRD"**: the CRD wasn't installed. Reapply: `kubectl apply -f <release.yaml-or-equivalent>`.
- **CrashLoopBackOff with "lease create forbidden"**: the leader-election Role/RoleBinding in the operator's namespace is missing.
- **Exits with "--webhook-enabled requires POD_NAMESPACE"**: the Deployment lacks the downward-API `POD_NAMESPACE` env var (the chart sets it).

---

## How do I tell whether the operator is healthy?

A four-line health check:

```bash
kubectl get deploy -n kube-vnet-system kube-vnet \
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
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet

# Just the baselines
kubectl get networkpolicy -A -l kube-vnet.system/managed-by=kube-vnet,kube-vnet.system/role=baseline

# Just the membership policies for a specific vnet
kubectl get networkpolicy -A -l kube-vnet.system/network=<home-ns>.<vnet-name>

# What's the operator running with?
kubectl get deploy -n kube-vnet-system kube-vnet \
  -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'

# Operator version
kubectl get deploy -n kube-vnet-system kube-vnet \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Live operator logs
kubectl logs -n kube-vnet-system deploy/kube-vnet -f

# Just errors
kubectl logs -n kube-vnet-system deploy/kube-vnet --tail=1000 \
  | jq -c 'select(.level=="error")'

# Recent Warning events on a vnet
kubectl get events -n <ns> --field-selector type=Warning,involvedObject.kind=VirtualNetwork

# Did a labeled pod get rejected anywhere?
kubectl get vnet -A -o json \
  | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name): \(.status.conditions[]? | select(.type=="Degraded" and .status=="True") | .message)"'
```
