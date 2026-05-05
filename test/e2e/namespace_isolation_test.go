//go:build e2e_namespace

// Package e2e — namespace-isolation posture tests. Runs against a kind
// cluster with kube-vnet installed at `operator.clusterBaseline.
// ingressIsolationLevel=namespace` (seeded ClusterVirtualNetworkBaseline:
// `namespace=default-both, cluster=default-egress`). Asserts the contract
// that posture introduces:
//
//   - Same-NS pods reach each other without any join label (the per-NS
//     `namespace` system vnet auto-applies at default-both).
//   - Cross-NS pods cannot reach each other in the absence of an explicit
//     user vnet (cluster=default-egress is asymmetric — sender can egress
//     but the receiver isn't on cluster as both/ingress, so the connection
//     is denied at the receiver).
//   - A pod that explicitly opts out via `kube-vnet/net.namespace=none`
//     becomes isolated even from same-NS peers.
//
// Build-tagged separately from the `e2e` suite so the existing pod-strict
// tests don't accidentally run against this posture (where their isolation
// assumptions wouldn't hold).
//
// Run via: KUBECONFIG=... go test -tags e2e_namespace ./test/e2e/... -v
package e2e

import (
	"testing"
	"time"
)

// TestE2E_NamespaceIsolation_SameNS_Reachable_NoLabel: two unlabeled pods
// in the same namespace can reach each other. Validates that the seeded
// cluster baseline auto-joins every pod to the per-NS `namespace` system
// vnet at default-both.
func TestE2E_NamespaceIsolation_SameNS_Reachable_NoLabel(t *testing.T) {
	ns := uniqueNS(t, "ns-iso-same")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	applyYAML(t, httpServerPod(ns, "server", nil))
	applyYAML(t, clientPod(ns, "client", nil))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	ip := podIP(t, ns, "server")
	if !canReach(t, ns, "client", ip, allowProbe) {
		t.Fatalf("expected unlabeled client → unlabeled server (same NS) to succeed under namespace isolation; got blocked")
	}
}

// TestE2E_NamespaceIsolation_CrossNS_NotReachable_Default: pods in
// different namespaces cannot reach each other when no user vnet bridges
// them. The cluster system vnet is at default-egress only — senders can
// egress, but the receiver isn't on cluster as both/ingress, so the
// connection is denied at the receiver's end.
func TestE2E_NamespaceIsolation_CrossNS_NotReachable_Default(t *testing.T) {
	homeNS := uniqueNS(t, "ns-iso-server")
	foreignNS := uniqueNS(t, "ns-iso-client")
	ensureNamespace(t, homeNS, nil)
	ensureNamespace(t, foreignNS, nil)
	defer cleanupNamespace(t, homeNS)
	defer cleanupNamespace(t, foreignNS)

	applyYAML(t, httpServerPod(homeNS, "server", nil))
	applyYAML(t, clientPod(foreignNS, "client", nil))
	waitForPod(t, homeNS, "server", 90*time.Second)
	waitForPod(t, foreignNS, "client", 90*time.Second)

	// Give the operator + CNI a moment to install the deny policies.
	time.Sleep(5 * time.Second)

	ip := podIP(t, homeNS, "server")
	if !cannotReach(t, foreignNS, "client", ip, denyProbe) {
		t.Fatalf("expected cross-NS client → server to be blocked under namespace isolation; got reachable")
	}
}

// TestE2E_NamespaceIsolation_PodOptOut_NoneIsolates: a pod with the
// explicit `kube-vnet/net.namespace=none` opt-out label becomes isolated
// from same-NS peers, even though the cluster baseline would otherwise
// auto-join it. Validates the per-vnet `=none` escape hatch against the
// inherited default.
func TestE2E_NamespaceIsolation_PodOptOut_NoneIsolates(t *testing.T) {
	ns := uniqueNS(t, "ns-iso-optout")
	ensureNamespace(t, ns, nil)
	defer cleanupNamespace(t, ns)

	// Server explicitly opts out; client stays on the inherited default.
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.namespace": "none"}))
	applyYAML(t, clientPod(ns, "client", nil))
	waitForPod(t, ns, "server", 90*time.Second)
	waitForPod(t, ns, "client", 90*time.Second)

	time.Sleep(5 * time.Second)

	ip := podIP(t, ns, "server")
	if !cannotReach(t, ns, "client", ip, denyProbe) {
		t.Fatalf("expected client → opted-out server to be blocked; got reachable")
	}
}
