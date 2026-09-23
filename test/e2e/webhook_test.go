//go:build e2e_webhook

package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Runs in the webhook entry of the e2e-helm matrix, next to the full suite:
// connectivity with the webhook on is covered there. This file only checks
// what the webhook itself guarantees.

// The webhook stamps a pod during admission, so the stamp is already in the
// create response. That is all it guarantees: whether the pod's first
// connection succeeds also depends on how fast the CNI programs the pod's IP
// (seconds on kube-router under load), so no single-shot connection check
// here - it would measure the CNI, not kube-vnet.
func TestWebhook_StampInCreateResponse(t *testing.T) {
	ns := uniqueNS(t, "wh")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	cmd := exec.Command("kubectl", "create", "-f", "-", "-o",
		`jsonpath={.metadata.labels.kube-vnet\.system/net\.`+ns+`\.net1} {.metadata.annotations.kube-vnet\.system/resolved-by}`)
	cmd.Stdin = strings.NewReader(clientPod(ns, "probe", map[string]string{"kube-vnet/net.net1": "both"}))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl create: %v\n%s", err, errOut.String())
	}

	if got := strings.TrimSpace(out.String()); got != "both admission" {
		t.Fatalf("create response carried stamp and resolved-by %q, want %q.\n"+
			"An empty stamp means the webhook did not see the pod: check that the "+
			"MutatingWebhookConfiguration selects this namespace and the operator is "+
			"serving on 9443.", got, "both admission")
	}
}
