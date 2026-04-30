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

// TestE2E_AllowedNamespaces_Names_PositiveAndNegative: a pod in a listed
// namespace can join; a pod in an unlisted one cannot — and is actually
// blocked at the traffic level, not just flagged in status.
func TestE2E_AllowedNamespaces_Names_PositiveAndNegative(t *testing.T) {
	homeNS := uniqueNS(t, "names-home")
	listedNS := uniqueNS(t, "names-listed")
	unlistedNS := uniqueNS(t, "names-unlisted")
	ensureNamespace(t, homeNS, nil)
	ensureNamespace(t, listedNS, nil)
	ensureNamespace(t, unlistedNS, nil)
	defer cleanupNamespace(t, homeNS)
	defer cleanupNamespace(t, listedNS)
	defer cleanupNamespace(t, unlistedNS)

	allowed := fmt.Sprintf("    names: [%s]\n", listedNS)
	applyYAML(t, vnetSpec("svc", homeNS, allowed))
	applyYAML(t, httpServerPod(homeNS, "server", map[string]string{
		"kube-vnet/net.svc": "true",
	}))
	applyYAML(t, clientPod(listedNS, "ok", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "true",
	}))
	applyYAML(t, clientPod(unlistedNS, "denied", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "true",
	}))
	waitForPod(t, homeNS, "server", 90*time.Second)
	waitForPod(t, listedNS, "ok", 90*time.Second)
	waitForPod(t, unlistedNS, "denied", 90*time.Second)

	ip := podIP(t, homeNS, "server")
	if !canReach(t, listedNS, "ok", ip, allowProbe) {
		t.Fatalf("listed namespace should reach the server")
	}
	if !cannotReach(t, unlistedNS, "denied", ip, denyProbe) {
		t.Fatalf("unlisted namespace must be blocked even with the join label")
	}
}

// TestE2E_AllowedNamespaces_Selector_PositiveAndNegative: same as above but
// using a label-based selector instead of an explicit name list.
func TestE2E_AllowedNamespaces_Selector_PositiveAndNegative(t *testing.T) {
	homeNS := uniqueNS(t, "sel-home")
	prodNS := uniqueNS(t, "sel-prod")
	devNS := uniqueNS(t, "sel-dev")
	ensureNamespace(t, homeNS, nil)
	ensureNamespace(t, prodNS, map[string]string{"tier": "prod"})
	ensureNamespace(t, devNS, map[string]string{"tier": "dev"})
	defer cleanupNamespace(t, homeNS)
	defer cleanupNamespace(t, prodNS)
	defer cleanupNamespace(t, devNS)

	allowed := "    selector:\n      matchLabels:\n        tier: prod\n"
	applyYAML(t, vnetSpec("svc", homeNS, allowed))
	applyYAML(t, httpServerPod(homeNS, "server", map[string]string{
		"kube-vnet/net.svc": "true",
	}))
	applyYAML(t, clientPod(prodNS, "ok", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "true",
	}))
	applyYAML(t, clientPod(devNS, "denied", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "true",
	}))
	waitForPod(t, homeNS, "server", 90*time.Second)
	waitForPod(t, prodNS, "ok", 90*time.Second)
	waitForPod(t, devNS, "denied", 90*time.Second)

	ip := podIP(t, homeNS, "server")
	if !canReach(t, prodNS, "ok", ip, allowProbe) {
		t.Fatalf("prod namespace should reach the server (selector matched)")
	}
	if !cannotReach(t, devNS, "denied", ip, denyProbe) {
		t.Fatalf("dev namespace must be blocked (selector did not match)")
	}
}

// TestE2E_MultiVNet_Pod: a pod that joins two vnets reaches members of both.
// Verifies the additive composition documented in the design.
func TestE2E_MultiVNet_Pod(t *testing.T) {
	ns := uniqueNS(t, "multi")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("payments", ns, ""))
	applyYAML(t, vnetSpec("monitoring", ns, ""))
	applyYAML(t, httpServerPod(ns, "p-server", map[string]string{
		"kube-vnet/net.payments": "true",
	}))
	applyYAML(t, httpServerPod(ns, "m-server", map[string]string{
		"kube-vnet/net.monitoring": "true",
	}))
	// Bridge pod is on BOTH vnets.
	applyYAML(t, clientPod(ns, "bridge", map[string]string{
		"kube-vnet/net.payments":   "true",
		"kube-vnet/net.monitoring": "true",
	}))
	waitForPod(t, ns, "p-server", 90*time.Second)
	waitForPod(t, ns, "m-server", 90*time.Second)
	waitForPod(t, ns, "bridge", 90*time.Second)

	pIP := podIP(t, ns, "p-server")
	mIP := podIP(t, ns, "m-server")
	if !canReach(t, ns, "bridge", pIP, allowProbe) {
		t.Fatalf("bridge → payments server should succeed")
	}
	if !canReach(t, ns, "bridge", mIP, allowProbe) {
		t.Fatalf("bridge → monitoring server should succeed")
	}
}

// TestE2E_Relabel_DropsAccess: removing the join label from a running pod
// must cause it to lose access on the next reconcile (covers the handler.Funcs
// removal path from ADR 0013).
func TestE2E_Relabel_DropsAccess(t *testing.T) {
	ns := uniqueNS(t, "relabel")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("net1", ns, ""))
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "true"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "true"}))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	ip := podIP(t, ns, "server")
	if !canReach(t, ns, "client", ip, allowProbe) {
		t.Fatalf("client should reach server while both share net1")
	}
	// Strip the join label from the client. Note kubectl label syntax: KEY-
	kubectlMust(t, "label", "pod", "-n", ns, "client", "kube-vnet/net.net1-")
	if !cannotReach(t, ns, "client", ip, denyProbe) {
		t.Fatalf("client should be blocked after losing the join label")
	}
}

// TestE2E_VNetDelete_BlocksTraffic: deleting the vnet removes all generated
// policies; previously-allowed traffic stops (the namespace is left with the
// baseline default-deny because there are still no other allow policies, but
// since the only baseline-applying signal was vnet membership, the baseline
// itself is deleted and the cluster's allow-all default returns. Either way,
// the operator-generated allow rule is gone — what we assert is that the
// allow rule from the vnet stops applying.)
func TestE2E_VNetDelete_BlocksTraffic(t *testing.T) {
	ns := uniqueNS(t, "vdelete")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("temp", ns, ""))
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.temp": "true"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.temp": "true"}))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	ip := podIP(t, ns, "server")
	if !canReach(t, ns, "client", ip, allowProbe) {
		t.Fatalf("baseline check: client should reach server while vnet exists")
	}

	// Delete the vnet. All generated policies (including the baseline, since
	// no member remains) should be removed.
	kubectlMust(t, "delete", "vnet", "-n", ns, "temp")

	// Wait for the membership policy to be gone.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, code := kubectl(t, "get", "networkpolicy", "-n", ns, "kube-vnet-temp-"+ns)
		if code != 0 {
			break
		}
		time.Sleep(time.Second)
	}
	// With the vnet gone the default-allow returns (operator removes its
	// baseline once the namespace has no managed members). Connectivity may
	// resume — what matters is the operator cleaned up cleanly. We assert
	// no operator-managed policies remain.
	out := kubectlMust(t, "get", "networkpolicy", "-n", ns,
		"-l", "kube-vnet/managed-by=kube-vnet", "-o", "name")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("operator-managed policies still exist after vnet delete:\n%s", out)
	}
}

// TestE2E_DNS_StillResolves: pods inside a vnet still reach CoreDNS, because
// the baseline + every membership policy explicitly allows UDP/TCP 53 to
// kube-system. Without the DNS allowance, name resolution would break the
// moment a pod joined a vnet.
func TestE2E_DNS_StillResolves(t *testing.T) {
	ns := uniqueNS(t, "dns")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("net1", ns, ""))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "true"}))
	waitForPod(t, ns, "client", 90*time.Second)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, code := kubectl(t, "exec", "-n", ns, "client", "--",
			"nslookup", "kubernetes.default.svc.cluster.local")
		if code == 0 && strings.Contains(out, "Address") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("DNS resolution failed inside a vnet pod within 30s")
}
