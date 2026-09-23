//go:build e2e_webhook

package e2e

import (
	"fmt"
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

	worker := strings.TrimSpace(kubectlMust(t, "get", "nodes",
		"-l", "!node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[0].metadata.name}"))
	if worker == "" {
		t.Fatal("no worker node")
	}

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
