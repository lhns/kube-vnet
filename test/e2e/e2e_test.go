//go:build e2e

// Package e2e contains end-to-end tests that run against a real Kubernetes
// cluster (kind + Calico) with kube-vnet already deployed.
//
// Bootstrap is the responsibility of the runner (hack/e2e-up.sh or the
// e2e GitHub Actions workflow). These tests assume kubectl is on PATH
// and points at the e2e cluster.
//
// Run via: KUBECONFIG=... go test -tags e2e ./test/e2e/... -v
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	// Image used for test pods. Tiny, has /bin/sh, has wget.
	testImage = "registry.k8s.io/e2e-test-images/agnhost:2.43"

	// Connectivity probe windows.
	allowProbe = 30 * time.Second // canReach: as soon as one wget succeeds, allow is confirmed
	denyProbe  = 15 * time.Second // cannotReach: every wget must fail for this long
)

// vnetSpec returns a VirtualNetwork manifest with optional allowedNamespaces.
// `allowed` may be nil for "home only".
func vnetSpec(name, ns string, allowed string) string {
	allowedYAML := ""
	if allowed != "" {
		allowedYAML = "\n  allowedNamespaces:\n" + allowed
	}
	return fmt.Sprintf(`apiVersion: kube-vnet.lhns.de/v1alpha1
kind: VirtualNetwork
metadata:
  name: %s
  namespace: %s
spec:%s
`, name, ns, allowedYAML)
}

// httpServerPod returns a Pod that runs `agnhost netexec` (HTTP server on :80).
func httpServerPod(ns, name string, joinLabels map[string]string) string {
	labels := []string{fmt.Sprintf("app: %s", name)}
	for k, v := range joinLabels {
		labels = append(labels, fmt.Sprintf("%s: %q", k, v))
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    %s
spec:
  containers:
    - name: web
      image: %s
      args: ["netexec", "--http-port=80"]
      ports:
        - containerPort: 80
`, name, ns, strings.Join(labels, "\n    "), testImage)
}

// clientPod returns a Pod that sleeps; kubectl exec is used to drive wget.
func clientPod(ns, name string, joinLabels map[string]string) string {
	labels := []string{fmt.Sprintf("app: %s", name)}
	for k, v := range joinLabels {
		labels = append(labels, fmt.Sprintf("%s: %q", k, v))
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    %s
spec:
  containers:
    - name: client
      image: %s
      command: ["sleep", "3600"]
`, name, ns, strings.Join(labels, "\n    "), testImage)
}

func ensureNamespace(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	labelLines := []string{fmt.Sprintf("kubernetes.io/metadata.name: %s", name)}
	for k, v := range labels {
		labelLines = append(labelLines, fmt.Sprintf("%s: %s", k, v))
	}
	applyYAML(t, fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    %s
`, name, strings.Join(labelLines, "\n    ")))
}

func cleanupNamespace(t *testing.T, name string) {
	t.Helper()
	kubectl(t, "delete", "namespace", name, "--ignore-not-found", "--wait=false")
}

// TestE2E_SameVNet_Connectivity: two pods on the same vnet can reach each other.
func TestE2E_SameVNet_Connectivity(t *testing.T) {
	ns := uniqueNS(t, "same")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("net1", ns, ""))
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "true"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "true"}))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	ip := podIP(t, ns, "server")
	if !canReach(t, ns, "client", ip, allowProbe) {
		t.Fatalf("expected client → server to succeed within %s", allowProbe)
	}
}

// TestE2E_DifferentVNets_Isolated: pods on different vnets cannot reach each other.
func TestE2E_DifferentVNets_Isolated(t *testing.T) {
	ns := uniqueNS(t, "diff")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("payments", ns, ""))
	applyYAML(t, vnetSpec("monitoring", ns, ""))
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.payments": "true"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.monitoring": "true"}))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	// Give the operator + Calico a moment to install the deny policies.
	time.Sleep(5 * time.Second)

	ip := podIP(t, ns, "server")
	if !cannotReach(t, ns, "client", ip, denyProbe) {
		t.Fatalf("expected client → server to be blocked but a request succeeded")
	}
}

// TestE2E_NoVNet_Isolated: a pod with no join label is isolated by the baseline default-deny.
func TestE2E_NoVNet_Isolated(t *testing.T) {
	ns := uniqueNS(t, "novnet")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("net1", ns, ""))
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "true"}))
	applyYAML(t, clientPod(ns, "loner", nil))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "loner", 90*time.Second)

	time.Sleep(5 * time.Second)
	ip := podIP(t, ns, "server")
	if !cannotReach(t, ns, "loner", ip, denyProbe) {
		t.Fatalf("expected loner → server to be blocked by baseline default-deny")
	}
}

// TestE2E_AllowedNamespaces_All: a pod in a foreign namespace can join a vnet
// whose allowedNamespaces.all=true.
func TestE2E_AllowedNamespaces_All(t *testing.T) {
	homeNS := uniqueNS(t, "obs-home")
	foreignNS := uniqueNS(t, "obs-foreign")
	ensureNamespace(t, homeNS, nil)
	ensureNamespace(t, foreignNS, nil)
	defer cleanupNamespace(t, homeNS)
	defer cleanupNamespace(t, foreignNS)

	applyYAML(t, vnetSpec("observability", homeNS, "    all: true\n"))
	applyYAML(t, httpServerPod(homeNS, "server", map[string]string{
		"kube-vnet/net.observability": "true",
	}))
	applyYAML(t, clientPod(foreignNS, "client", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.observability", homeNS): "true",
	}))
	waitForPod(t, homeNS, "server", 90*time.Second)
	waitForPod(t, foreignNS, "client", 90*time.Second)

	ip := podIP(t, homeNS, "server")
	if !canReach(t, foreignNS, "client", ip, allowProbe) {
		t.Fatalf("expected foreign client → home server with allowedNamespaces.all=true")
	}
}
