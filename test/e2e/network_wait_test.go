//go:build e2e_webhook

package e2e

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Runs wherever the network wait is enabled: the kube-router entry of the
// e2e-helm matrix and the e2e-network-wait lanes (ADR 0045). E2E_CNI names
// the CNI.
//
// The server runs on the control-plane node and the clients on the worker, so
// the rule a client depends on is programmed by a node other than its own.
// Each client's app makes exactly one connection, with no retry: it only
// succeeds if the wait held the app until that remote node had applied the
// client. Clients without the annotation start at the same time; they are the
// race the wait exists for, so their result is logged, not asserted.
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
	client := func(name, annotation string) string {
		return pinnedPod(ns, name, fmt.Sprintf(`
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
    - name: client
      image: %s
      command: ["wget", "-q", "-T", "5", "-O", "/dev/null", "http://%s/"]`, worker, testImage, serverIP), annotation)
	}
	const clients = 5
	var manifests []string
	for i := range clients {
		manifests = append(manifests,
			client(fmt.Sprintf("wait-%d", i), `kube-vnet/network-max-wait: "60s"`),
			client(fmt.Sprintf("nowait-%d", i), ""))
	}
	applyYAML(t, strings.Join(manifests, "---\n"))

	unwaited := 0
	for i := range clients {
		if finalPhase(t, ns, fmt.Sprintf("nowait-%d", i), 2*time.Minute) == "Succeeded" {
			unwaited++
		}
	}
	for i := range clients {
		name := fmt.Sprintf("wait-%d", i)
		phase := finalPhase(t, ns, name, 2*time.Minute)
		waitLog, _ := kubectl(t, "logs", "-n", ns, name, "-c", "kube-vnet-network-wait")
		t.Logf("%s: %s; wait log:\n%s", name, phase, waitLog)
		if phase != "Succeeded" {
			appLog, _ := kubectl(t, "logs", "-n", ns, name, "-c", "client")
			t.Errorf("%s: the single connection did not succeed (%s); client log:\n%s", name, phase, appLog)
		}
		// Succeeded after a timeout would mean the connection won the race by
		// luck, not because the wait did its job.
		if !strings.Contains(waitLog, "beacons accepted after") {
			t.Errorf("%s: the wait did not release on its beacons", name)
		}
	}
	t.Logf("on %s, %d of %d clients without the wait connected on the first try", cni(), unwaited, clients)
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
// usually accept before the wait's first round. This one shows whether a
// beacon is a witness, i.e. whether it refuses a source its node's CNI
// doesn't know as a pod.
//
// A hostNetwork pod's source is its node's IP, which no NetworkPolicy peer
// {namespaceSelector: {}} covers, so the beacon on the other node must deny
// it: kube-router refuses, Calico and Cilium drop. The beacon on its own node
// is the control: CNIs let a node reach its local pods, so that connection
// shows the pod can reach a beacon at all.
// A normal pod on the same node must be accepted by the remote beacon. All
// three are observed and logged before anything is asserted, so one run shows
// how a CNI behaves.
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

	memberOut, memberOK := connectWithin(t, ns, "member", remote, time.Minute)
	localOut, localOK := connectWithin(t, ns, "hostnet", local, 30*time.Second)
	// Both pods have existed for as long as the remote node has known the
	// member, so a refusal below is not a matter of time.
	accepted, refused := 0, map[string]int{}
	for deadline := time.Now().Add(denyProbe); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if out, ok := connect(t, ns, "hostnet", remote); ok {
			accepted++
		} else {
			refused[out]++
		}
	}
	t.Logf("on %s: member -> remote beacon: %t (%s); hostNetwork -> local beacon: %t (%s); "+
		"hostNetwork -> remote beacon: accepted %d times, failed %v",
		cni(), memberOK, memberOut, localOK, localOut, accepted, refused)

	if !memberOK {
		t.Errorf("the remote beacon never accepted a pod its CNI knows")
	}
	if !localOK {
		t.Errorf("the hostNetwork pod could not reach its own node's beacon")
	}
	if accepted > 0 {
		t.Errorf("the remote beacon accepted a source no pod owns: it is not a witness")
	}
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

// finalPhase waits until pod has finished and returns its phase, or the last
// phase seen when timeout passes.
func finalPhase(t *testing.T, ns, pod string, timeout time.Duration) string {
	t.Helper()
	var phase string
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		phase = strings.TrimSpace(kubectlMust(t, "get", "pod", "-n", ns, pod, "-o", "jsonpath={.status.phase}"))
		if phase == "Succeeded" || phase == "Failed" {
			break
		}
	}
	return phase
}
