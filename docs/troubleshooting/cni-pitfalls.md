# CNI pitfalls that silently break NetworkPolicy enforcement

If pods that should be isolated can still reach each other, kube-vnet has likely done its job correctly — the issue is at the CNI or distro layer. The operator generates `NetworkPolicy` resources via the apiserver; the *enforcement* happens in the CNI's dataplane. When enforcement breaks, the policies still look right in `kubectl describe netpol`.

This page catalogs the misconfigurations and known bugs that cause silent enforcement failures, with how to confirm each.

For the symptom-first index, see [`../troubleshooting.md`](../troubleshooting.md). For policy semantics, see [`../concepts.md`](../concepts.md).

## Triage flow

1. **Confirm the operator's policies exist.**

   ```bash
   kubectl get netpol -A -l kube-vnet.system/managed-by=kube-vnet
   ```

   If the expected baseline + membership policies are missing, the issue is the operator (open the [troubleshooting index](../troubleshooting.md)). If they're present, continue.

2. **Confirm your CNI claims to enforce NetworkPolicy.** See [`../install.md`](../install.md) for compatible CNIs. Quick check on a node:

   ```bash
   crictl ps | grep -E 'calico-node|cilium|kube-router'
   ```

3. **Confirm the CNI's on-disk config matches its ConfigMap.** Several distros ship the CNI config via a ConfigMap that's written to `/etc/cni/net.d/` once at first boot. ConfigMap updates don't always propagate. See pitfall 2.

4. **Run a manual probe** (recipe at the bottom of this page).

If steps 1–3 all check out, the policies exist *and* the CNI is running, isolation should be working. If a manual probe still passes traffic that shouldn't pass, you've hit one of the pitfalls below.

---

## Pitfall 1: kube-router with `ipMasq: true`

**Symptom.** Pod-to-pod traffic, even within the same namespace, isn't blocked despite the membership `NetworkPolicy` selecting the right pods. `tcpdump` on the destination shows the source IP is a *node IP*, not the source pod's IP.

**Cause.** kube-router's bridge CNI plugin masquerades intra-cluster traffic behind the node IP when `ipMasq: true`. NetworkPolicy `podSelector` matching depends on the source-pod IP being preserved end-to-end; once SNAT'd to the node IP, the lookup against the apiserver's pod cache fails and the policy's default-allow takes over (or, depending on the CNI's logic, the deny rule is never matched at all).

This is *not* the same setting as kube-router's DaemonSet `--masquerade-all` flag. The `ipMasq` field is on the bridge plugin in `cni-conf.json`; `--masquerade-all` is a kube-router-process flag. They both produce SNAT, but the bridge plugin's `ipMasq` does it inside the CNI (so kube-router's NPC enforcement runs *after* SNAT and sees the wrong source).

**Fix on k0s.** Set `spec.network.kuberouter.ipMasq: false` in `k0sctl.yaml`. Note: changes here do not always propagate after first bootstrap — see pitfall 2.

**Fix on a manually-installed kube-router.** Edit the `kube-router-cfg` ConfigMap directly:

```bash
kubectl edit cm kube-router-cfg -n kube-system
# In .data["cni-conf.json"], find the bridge plugin entry and set "ipMasq": false.
```

Then either delete the on-disk file on each node and restart the DaemonSet (see pitfall 2's workaround) or accept that kube-router will pick up the change on the next first-boot of each node.

**How to verify.** On any worker:

```bash
sudo cat /etc/cni/net.d/10-kuberouter.conflist | jq '.plugins[] | select(.type=="bridge") | .ipMasq'
# expect: false
```

---

## Pitfall 2: k0s — `kuberouter.*` ConfigMap fields don't propagate after first bootstrap

**Symptom.** You change `kuberouter.ipMasq` (or `mtu`, `hairpinMode`) in `k0sctl.yaml`, run `k0sctl apply`. The `kube-router-cfg` ConfigMap is updated correctly, but each worker's `/etc/cni/net.d/10-kuberouter.conflist` still has the old value. Pod restarts and `kubectl rollout restart ds/kube-router -n kube-system` don't help.

**Cause.** k0s's kube-router DaemonSet has an `install-cniconf` init container that runs:

```sh
if [ ! -f /etc/cni/net.d/10-kuberouter.conflist ]; then
  ...copy ConfigMap content to host file...
fi
```

The guard means: if the file exists (which it does after first bootstrap), the init container is a no-op. The script writes the file once at first boot and never updates it. The `KubeRouter.Reconcile()` controller path on k0s does notice the config change and updates the ConfigMap — but the on-disk file isn't refreshed because of the init-container guard.

A 2023 fix (PR #2829) sidestepped this for `autoMTU` by moving it out of the ConfigMap into kube-router CLI args. The class of bug remains for `ipMasq`, `mtu`, and `hairpinMode`.

**Workaround.** Delete the on-disk file on every node, then roll the DaemonSet:

```bash
for n in <node1> <node2> ...; do
  ssh $n 'sudo rm -f /etc/cni/net.d/10-kuberouter.conflist'
done
kubectl rollout restart ds/kube-router -n kube-system
```

Or write the ConfigMap content directly to each node:

```bash
CONFLIST=$(kubectl get cm kube-router-cfg -n kube-system -o jsonpath='{.data.cni-conf\.json}')
for n in <node1> <node2> ...; do
  echo "$CONFLIST" | ssh $n 'sudo tee /etc/cni/net.d/10-kuberouter.conflist >/dev/null'
done
```

Either way, the fix is per-node. There is no current k0s-level mechanism to force re-propagation.

Track upstream: this should be filed against `k0sproject/k0s` if not already (see also k0s issue #2822 for the autoMTU precedent).

---

## Pitfall 3: kube-router service-proxy + node reboot bootstrap deadlock

**Symptom.** With `kubeProxy.disabled: true` and kube-router running with `--run-service-proxy=true`, the cluster works fine until a worker node reboot. After reboot, the kube-router pod enters `CrashLoopBackOff`:

```
dial tcp 172.19.0.1:443: i/o timeout
failed to run kube-router: failed to synchronize cache: 1m0s timeout
```

**Cause.** Bootstrap deadlock:

1. kube-router needs to list Services / EndpointSlices from the apiserver to know what IPVS rules to program.
2. The default in-cluster client targets the cluster API VIP (`KUBERNETES_SERVICE_HOST`/`PORT`).
3. That VIP needs an IPVS rule to be reachable.
4. The component that programs that rule is kube-router itself.

Before reboot this works because IPVS was programmed earlier (e.g. while kube-proxy was still running, or by kube-router on a live cluster). The kernel IPVS table persists across pod restarts but **not** across host reboots. After a reboot, IPVS is empty and kube-router has no path to bring itself up.

Konnectivity does *not* help: it's a control-plane → worker tunnel, not the reverse direction.

**Why this is a k0s-shaped gap, not a kube-router bug.** k0s manages the kube-router DaemonSet template and doesn't currently expose a way to point kube-router at a non-VIP apiserver endpoint (e.g. the controller LB at `spec.api.externalAddress`, or the local NLLB Envoy at `127.0.0.1:7443`). In a from-scratch kube-router deployment, the operator would set `KUBERNETES_SERVICE_HOST`/`PORT` env vars or pass a custom `--kubeconfig`. k0s users can't.

Tracked upstream as k0s issue #3943 (open since Jan 2024, no maintainer engagement at last check).

**Recovery.**

1. Re-enable kube-proxy in `k0sctl.yaml` (set `spec.network.kubeProxy.disabled: false`); remove `--run-service-proxy=true` from kube-router args.
2. `k0sctl apply`. Control plane is unaffected by worker degradation, so this works.
3. kube-proxy pods come up, program iptables for the API VIP, kube-router unwedges.
4. On each worker, install `ipvsadm` and run `sudo ipvsadm -C` to flush stale kube-router IPVS rules. Optionally `sudo ip link delete kube-dummy-if`.
5. `kubectl rollout restart ds/kube-router -n kube-system` to clear in-memory state.

**Long-term.** Don't run kube-router as service proxy on k0s until #3943 lands. The supported configuration is kube-router as CNI + router + firewall + NPC only, with kube-proxy enabled.

---

## Pitfall 4: Calico — Felix not running

Felix is the per-node agent that programs iptables / nftables for Calico's policy enforcement. If Felix isn't running, NetworkPolicy resources exist but nothing enforces them.

**How to verify.**

```bash
kubectl -n kube-system get pod -l k8s-app=calico-node
calicoctl node status            # if calicoctl is installed
kubectl -n kube-system logs -l k8s-app=calico-node --tail=200 | grep -i 'felix\|policy'
```

For deeper diagnosis, see Calico's [troubleshooting guide](https://docs.tigera.io/calico/latest/operations/troubleshoot/).

---

## Pitfall 5: Cilium — identity-allocation lag

Cilium translates `podSelector`s into "security identities" that it caches on each node. Newly-started pods can briefly hit a window where the identity hasn't been allocated yet, and policies select them inconsistently.

**Symptom.** First few seconds after a pod starts, traffic that should be isolated isn't (or vice versa). After ~5–30 seconds, things settle.

This is documented behavior, not a bug. Cilium provides metrics (`cilium_identity_allocation_attempts_total`, `cilium_endpoint_state`) for visibility. See Cilium's [identity-management docs](https://docs.cilium.io/en/stable/network/concepts/security-identities/).

If pods routinely send hard traffic immediately on startup and you can't tolerate the lag, look at Cilium's `--identity-allocation-mode=crd` vs. `kvstore` choice and the `--enable-well-known-identities` flag.

---

## Manual isolation probe recipe

If you've checked all of the above and want to confirm a `NetworkPolicy` is actually being enforced:

```bash
NS=<your-vnet-home-ns>
VNET=<your-vnet-name>

# A pod IN the vnet, accepts ingress.
kubectl run member -n "$NS" \
  --image=nicolaka/netshoot \
  --labels="kube-vnet/net.${VNET}=both" \
  --command -- sleep 1d

# A pod NOT in the vnet, in the same namespace.
kubectl run outsider -n "$NS" \
  --image=nicolaka/netshoot \
  --command -- sleep 1d

kubectl wait -n "$NS" pod/member pod/outsider --for=condition=Ready --timeout=60s

MEMBER_IP=$(kubectl get -n "$NS" pod/member -o jsonpath='{.status.podIP}')

# Start a listener on the member.
kubectl exec -n "$NS" member -- nc -l -p 9090 &

# From the outsider, try to reach it.
kubectl exec -n "$NS" outsider -- timeout 3 nc -vz "$MEMBER_IP" 9090
# Expect: "timed out" or "connection refused" if isolation works.
# If "succeeded", the CNI is not enforcing the policy.

kubectl delete pod -n "$NS" member outsider
```

A future opt-in `kube-vnet verify` subcommand will automate this — see [ADR 0028](../adr/0028-runtime-policy-verification.md) for the design.
