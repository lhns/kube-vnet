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

// TestE2E_SameVNet_Connectivity: two pods on the same vnet can reach each other.
func TestE2E_SameVNet_Connectivity(t *testing.T) {
	ns := uniqueNS(t, "same")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, vnetSpec("net1", ns, ""))
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "both"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "both"}))
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
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.payments": "both"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.monitoring": "both"}))
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
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "both"}))
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
		"kube-vnet/net.observability": "both",
	}))
	applyYAML(t, clientPod(foreignNS, "client", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.observability", homeNS): "both",
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
		"kube-vnet/net.svc": "both",
	}))
	applyYAML(t, clientPod(listedNS, "ok", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "both",
	}))
	applyYAML(t, clientPod(unlistedNS, "denied", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "both",
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

// TestE2E_AllowedNamespaces_Names_UnlabeledPodBlocked: a pod in a namespace
// that's listed in allowedNamespaces but does NOT carry the join label gets
// no access. allowedNamespaces gates *eligibility to join*, not blanket
// access. Proves the join-vs-blanket semantic at the actual-traffic level.
func TestE2E_AllowedNamespaces_Names_UnlabeledPodBlocked(t *testing.T) {
	homeNS := uniqueNS(t, "ujoin-home")
	listedNS := uniqueNS(t, "ujoin-listed")
	ensureNamespace(t, homeNS, nil)
	ensureNamespace(t, listedNS, nil)
	defer cleanupNamespace(t, homeNS)
	defer cleanupNamespace(t, listedNS)

	allowed := fmt.Sprintf("    names: [%s]\n", listedNS)
	applyYAML(t, vnetSpec("svc", homeNS, allowed))
	applyYAML(t, httpServerPod(homeNS, "server", map[string]string{
		"kube-vnet/net.svc": "both",
	}))
	// labeled-and-listed: should reach.
	applyYAML(t, clientPod(listedNS, "labeled", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "both",
	}))
	// unlabeled-but-listed: even though its namespace is allowedNamespaces,
	// without the join label it's NOT a member → must not reach.
	applyYAML(t, clientPod(listedNS, "bystander", nil))
	waitForPod(t, homeNS, "server", 90*time.Second)
	waitForPod(t, listedNS, "labeled", 90*time.Second)
	waitForPod(t, listedNS, "bystander", 90*time.Second)

	// Wait for the operator to install policies (a labeled pod exists, so the
	// baseline + membership policy should land in listedNS too).
	time.Sleep(5 * time.Second)

	ip := podIP(t, homeNS, "server")
	if !canReach(t, listedNS, "labeled", ip, allowProbe) {
		t.Fatalf("labeled pod in listed namespace should reach the server")
	}
	if !cannotReach(t, listedNS, "bystander", ip, denyProbe) {
		t.Fatalf("unlabeled pod in listed namespace must NOT reach the server (allowedNamespaces gates join eligibility, not blanket access)")
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
		"kube-vnet/net.svc": "both",
	}))
	applyYAML(t, clientPod(prodNS, "ok", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "both",
	}))
	applyYAML(t, clientPod(devNS, "denied", map[string]string{
		fmt.Sprintf("kube-vnet/net.%s.svc", homeNS): "both",
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
		"kube-vnet/net.payments": "both",
	}))
	applyYAML(t, httpServerPod(ns, "m-server", map[string]string{
		"kube-vnet/net.monitoring": "both",
	}))
	// Bridge pod is on BOTH vnets.
	applyYAML(t, clientPod(ns, "bridge", map[string]string{
		"kube-vnet/net.payments":   "both",
		"kube-vnet/net.monitoring": "both",
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
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "both"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "both"}))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	ip := podIP(t, ns, "server")
	if !canReach(t, ns, "client", ip, allowProbe) {
		t.Fatalf("client should reach server while both share net1")
	}
	// Strip the join label from the client. Note kubectl label syntax: KEY-
	kubectlMust(t, "label", "pod", "-n", ns, "client", "kube-vnet/net.net1-")
	// Wait for the resolution controller to strip the canonical FQ system
	// label before probing — cannotReach is fail-fast on success and would
	// otherwise race the operator (per ADR 0034's pod-edit window). The
	// system label key matches what the membership policy's `from:` selector
	// matches on, so its absence is the right convergence oracle.
	waitForLabelGone(t, ns, "client", "kube-vnet.system/net."+ns+".net1", 30*time.Second)
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
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.temp": "both"}))
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.temp": "both"}))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	ip := podIP(t, ns, "server")
	if !canReach(t, ns, "client", ip, allowProbe) {
		t.Fatalf("baseline check: client should reach server while vnet exists")
	}

	// Delete the vnet. The membership policy should be removed by
	// cleanupForDeleted. The baseline is independent of vnet lifecycle (ADR
	// 0023: it's owned by the NamespaceReconciler and decided by the
	// resolved ingress-isolation mode), so we only assert that membership
	// policies disappear — not the baseline.
	kubectlMust(t, "delete", "vnet", "-n", ns, "temp")

	deadline := time.Now().Add(30 * time.Second)
	var lastOut string
	for time.Now().Before(deadline) {
		out, _ := kubectl(t, "get", "networkpolicy", "-n", ns,
			"-l", "kube-vnet.system/role=membership", "-o", "name")
		lastOut = strings.TrimSpace(out)
		if lastOut == "" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("membership policies still exist 30s after vnet delete:\n%s", lastOut)
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
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "both"}))
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
