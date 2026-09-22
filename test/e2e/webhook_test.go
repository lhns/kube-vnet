//go:build e2e_webhook

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// This lane runs with `webhook.enabled=true`; every other e2e lane runs with
// it off, so both configurations stay covered.

// The whole point of the admission webhook (ADR 0034): a pod is a member of
// its virtual networks before it runs, so its FIRST connection succeeds.
//
// Deliberately single-shot. canReach retries for 30s, which would hide
// exactly the failure under test — the window in which a just-started pod is
// denied because it is not yet stamped is well under a second, so any retry
// loop turns a real regression into a pass.
func TestWebhook_FirstConnectionSucceeds(t *testing.T) {
	ns := uniqueNS(t, "wh")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	// Server first, fully ready, so the only variable left is the client's
	// own membership at the moment it starts.
	applyYAML(t, httpServerPod(ns, "server", map[string]string{"kube-vnet/net.net1": "both"}))
	waitForPod(t, ns, "server", 120*time.Second)
	serverIP := podIP(t, ns, "server")

	// A brand-new client. Under the webhook it is stamped during admission,
	// so the allow rule already matches it when it starts.
	applyYAML(t, clientPod(ns, "client", map[string]string{"kube-vnet/net.net1": "both"}))
	waitForPod(t, ns, "client", 120*time.Second)

	out, code := kubectl(t, "exec", "-n", ns, "client", "--",
		"wget", "-q", "-T", "5", "-O", "-", fmt.Sprintf("http://%s/", serverIP))
	if code != 0 {
		t.Fatalf("the first connection from a freshly started pod was denied (rc=%d, out=%q).\n"+
			"With the admission webhook enabled the pod must carry its membership stamp "+
			"before it runs; a failure here means resolution is happening after start "+
			"again, which is the race ADR 0034 exists to remove.\n"+
			"Inspect with:\n"+
			"  kubectl get pod -n %s client -o jsonpath='{.metadata.labels}'\n"+
			"  kubectl get pod -n %s client -o jsonpath='{.metadata.annotations}'",
			code, out, ns, ns)
	}
}

// The stamp must come from admission, not from the controller catching up.
// Without this, the test above could pass purely because the operator happened
// to be fast — which is the condition we are trying to stop relying on.
func TestWebhook_StampAppliedByAdmission(t *testing.T) {
	ns := uniqueNS(t, "whann")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	applyYAML(t, clientPod(ns, "probe", map[string]string{"kube-vnet/net.net1": "both"}))

	out, code := kubectl(t, "get", "pod", "-n", ns, "probe", "-o",
		`jsonpath={.metadata.annotations.kube-vnet\.system/resolved-by}`)
	if code != 0 {
		t.Fatalf("reading resolved-by: rc=%d out=%q", code, out)
	}
	if out != "admission" {
		t.Fatalf("resolved-by=%q, want \"admission\".\n"+
			"The pod was stamped by the controller, so the webhook never saw it: check that "+
			"the MutatingWebhookConfiguration selects this namespace and that the operator "+
			"is serving on 9443.", out)
	}
}
