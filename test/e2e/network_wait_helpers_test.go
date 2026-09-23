//go:build e2e_webhook || e2e_experiment

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Helpers shared by the network wait tests and the beacon-ordering
// experiment.

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

// cni names the CNI the lane installed, for logs and per-CNI skips.
func cni() string {
	if c := os.Getenv("E2E_CNI"); c != "" {
		return c
	}
	return "an unnamed CNI"
}
