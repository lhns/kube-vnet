//go:build e2e_webhook

package e2e

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Runs in the kube-router entry of the e2e-helm matrix, which installs with
// webhook.networkWait.enabled (ADR 0045).
//
// The server runs on the control-plane node and the client on the worker, so
// the rule the client depends on is programmed by a node other than its own.
// The client's app makes exactly one connection, with no retry: it only
// succeeds if the wait held the app until that remote node had applied the
// client. Without the annotation the same pod is the race this feature
// exists for, so its connection result is not asserted.
func TestNetworkWait_FirstConnectionSucceeds(t *testing.T) {
	ns := uniqueNS(t, "nwait")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	applyYAML(t, pinnedPod(ns, "server", `
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
  containers:
    - name: web
      image: `+testImage+`
      args: ["netexec", "--http-port=80"]`, ""))
	waitForPod(t, ns, "server", 2*time.Minute)
	serverIP := podIP(t, ns, "server")

	worker := workerNode(t)

	const maxWait = "60s"
	applyYAML(t, pinnedPod(ns, "client", fmt.Sprintf(`
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
    - name: client
      image: %s
      command: ["wget", "-q", "-T", "5", "-O", "/dev/null", "http://%s/"]`, worker, testImage, serverIP),
		`kube-vnet/network-max-wait: "`+maxWait+`"`))

	out, code := kubectl(t, "wait", "-n", ns, "pod/client",
		"--for=jsonpath={.status.phase}=Succeeded", "--timeout=120s")
	waitLog, _ := kubectl(t, "logs", "-n", ns, "client", "-c", "kube-vnet-network-wait")
	if code != 0 {
		appLog, _ := kubectl(t, "logs", "-n", ns, "client", "-c", "client")
		t.Fatalf("the client's single connection did not succeed: %s\nwait log:\n%s\nclient log:\n%s",
			out, waitLog, appLog)
	}
	// Succeeded after a timeout would mean the wait didn't do its job and the
	// connection won the race by luck.
	if !strings.Contains(waitLog, "beacons accepted after") {
		t.Fatalf("the wait did not release on its beacons before %s:\n%s", maxWait, waitLog)
	}
	t.Logf("wait log:\n%s", waitLog)
}

// Without the annotation, nothing is injected.
func TestNetworkWait_NotInjectedWithoutAnnotation(t *testing.T) {
	ns := uniqueNS(t, "nwait")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	applyYAML(t, clientPod(ns, "plain", map[string]string{"kube-vnet/net.net1": "both"}))
	if inits := kubectlMust(t, "get", "pod", "-n", ns, "plain",
		"-o", "jsonpath={.spec.initContainers[*].name}"); strings.TrimSpace(inits) != "" {
		t.Fatalf("init containers injected into a pod that did not opt in: %s", inits)
	}
}

// The first test passes whether or not a beacon ever refuses: the beacons
// usually accept before the wait's first round. This one shows that a beacon
// is a witness, i.e. that it refuses a source its node's CNI doesn't know.
//
// A hostNetwork pod's source is its node's IP, which kube-router puts in no
// pod ipset, so the beacon on the other node must refuse it. The beacon on its
// own node is the control: kube-router accepts anything from a pod's local
// node, so that connection proves the pod can reach a beacon at all. A normal
// pod on the same node is accepted by the remote beacon.
func TestNetworkWait_BeaconRefusesUnknownSource(t *testing.T) {
	ns := uniqueNS(t, "nwait")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	worker := workerNode(t)
	sleeper := func(hostNetwork bool) string {
		return fmt.Sprintf(`
  hostNetwork: %t
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
    - name: client
      image: %s
      command: ["sleep", "3600"]`, hostNetwork, worker, testImage)
	}
	applyYAML(t, pinnedPod(ns, "hostnet", sleeper(true), ""))
	applyYAML(t, pinnedPod(ns, "member", sleeper(false), ""))
	waitForPod(t, ns, "hostnet", 2*time.Minute)
	waitForPod(t, ns, "member", 2*time.Minute)

	var local, remote string
	for node, ip := range beaconsByNode(t) {
		if node == worker {
			local = net.JoinHostPort(ip, "9444")
		} else {
			remote = net.JoinHostPort(ip, "9444")
		}
	}
	if local == "" || remote == "" {
		t.Fatalf("want a beacon on %s and one on another node, got local %q remote %q", worker, local, remote)
	}

	if out, ok := connectWithin(t, ns, "member", remote, time.Minute); !ok {
		t.Fatalf("the remote beacon never accepted a pod its CNI knows: %s", out)
	}
	if out, ok := connectWithin(t, ns, "hostnet", local, 30*time.Second); !ok {
		t.Fatalf("the hostNetwork pod could not reach its own node's beacon: %s", out)
	}
	// Both pods have existed for as long as the remote node has known the
	// member, so the refusal below is not a matter of time.
	deadline := time.Now().Add(denyProbe)
	var last string
	for time.Now().Before(deadline) {
		out, ok := connect(t, ns, "hostnet", remote)
		if ok {
			t.Fatalf("the remote beacon accepted a source no pod owns (%s): it is not a witness", out)
		}
		last = out
		time.Sleep(2 * time.Second)
	}
	t.Logf("remote beacon %s refused the hostNetwork pod for %s: %s", remote, denyProbe, last)
}

// pinnedPod is a member of vnet net1 with the given spec body and, optionally,
// one annotation line.
func pinnedPod(ns, name, spec, annotation string) string {
	annotations := ""
	if annotation != "" {
		annotations = "\n  annotations:\n    " + annotation
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    %s%s
spec:%s
`, name, ns, podLabelsYAML(name, map[string]string{"kube-vnet/net.net1": "both"}), annotations, spec)
}

// workerNode returns the name of the cluster's worker node.
func workerNode(t *testing.T) string {
	t.Helper()
	worker := strings.TrimSpace(kubectlMust(t, "get", "nodes",
		"-l", "!node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[0].metadata.name}"))
	if worker == "" {
		t.Fatal("no worker node")
	}
	return worker
}

// beaconsByNode maps each node to its beacon's pod IP.
func beaconsByNode(t *testing.T) map[string]string {
	t.Helper()
	kubectlMust(t, "rollout", "status", "-n", "kube-vnet-system",
		"ds/kube-vnet-controller-network-beacon", "--timeout=120s")
	out := kubectlMust(t, "get", "pods", "-n", "kube-vnet-system",
		"-l", "app.kubernetes.io/component=network-beacon",
		"-o", "jsonpath={range .items[*]}{.spec.nodeName}={.status.podIP} {end}")
	beacons := map[string]string{}
	for _, pair := range strings.Fields(out) {
		if node, ip, ok := strings.Cut(pair, "="); ok && ip != "" {
			beacons[node] = ip
		}
	}
	return beacons
}

// connect opens one TCP connection from pod to addr.
func connect(t *testing.T, ns, pod, addr string) (string, bool) {
	t.Helper()
	out, code := kubectl(t, "exec", "-n", ns, pod, "--", "/agnhost", "connect", "--timeout=2s", addr)
	return strings.TrimSpace(out), code == 0
}

// connectWithin retries connect until it succeeds or timeout passes.
func connectWithin(t *testing.T, ns, pod, addr string, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, ok := connect(t, ns, pod, addr)
		if ok || time.Now().After(deadline) {
			return out, ok
		}
		time.Sleep(time.Second)
	}
}
