//go:build e2e || e2e_namespace || e2e_webhook

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/rand"
)

const (
	// Image used for test pods: agnhost, plus /bin/sh and wget.
	testImage = "registry.k8s.io/e2e-test-images/agnhost:2.43"

	// Connectivity probe windows.
	allowProbe = 30 * time.Second // canReach: as soon as one wget succeeds, allow is confirmed
	denyProbe  = 15 * time.Second // cannotReach: every wget must fail for this long
)

// vnetSpec returns a VirtualNetwork manifest. `allowed` is the indented body
// of spec.allowedNamespaces, or "" for home-namespace-only.
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

// podLabelsYAML renders `app: <name>` plus joinLabels as indented YAML lines.
func podLabelsYAML(name string, joinLabels map[string]string) string {
	labels := []string{fmt.Sprintf("app: %s", name)}
	for k, v := range joinLabels {
		labels = append(labels, fmt.Sprintf("%s: %q", k, v))
	}
	return strings.Join(labels, "\n    ")
}

// httpServerPod returns a Pod that runs `agnhost netexec` (HTTP server on :80).
func httpServerPod(ns, name string, joinLabels map[string]string) string {
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
`, name, ns, podLabelsYAML(name, joinLabels), testImage)
}

// clientPod returns a Pod that sleeps; kubectl exec is used to drive wget.
func clientPod(ns, name string, joinLabels map[string]string) string {
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
`, name, ns, podLabelsYAML(name, joinLabels), testImage)
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

// kubectl runs `kubectl ARGS...` and returns combined stdout+stderr and exit code.
func kubectl(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return buf.String(), 0
	case errors.As(err, &ee):
		return buf.String(), ee.ExitCode()
	default:
		return buf.String(), -1
	}
}

// kubectlMust runs kubectl and fatals if exit != 0.
func kubectlMust(t *testing.T, args ...string) string {
	t.Helper()
	out, code := kubectl(t, args...)
	if code != 0 {
		t.Fatalf("kubectl %s failed (%d):\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// applyYAML pipes the given YAML to `kubectl apply -f -`.
func applyYAML(t *testing.T, yaml string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl apply failed: %v\n%s", err, buf.String())
	}
}

// waitForPod waits up to timeout for a pod to be Ready.
func waitForPod(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()
	out, code := kubectl(t, "wait", "-n", ns, "pod/"+name,
		"--for=condition=Ready", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if code != 0 {
		t.Fatalf("wait pod %s/%s: %s", ns, name, out)
	}
}

// podIP returns the IP of a pod (and fails if missing).
func podIP(t *testing.T, ns, name string) string {
	t.Helper()
	out := kubectlMust(t, "get", "pod", "-n", ns, name, "-o", "jsonpath={.status.podIP}")
	out = strings.TrimSpace(out)
	if out == "" {
		t.Fatalf("pod %s/%s has no IP", ns, name)
	}
	return out
}

// wget makes one HTTP request from srcPod to dstIP:80 and reports success.
func wget(t *testing.T, ns, srcPod, dstIP string) bool {
	t.Helper()
	_, code := kubectl(t, "exec", "-n", ns, srcPod, "--",
		"wget", "-q", "-T", "2", "-O", "-", fmt.Sprintf("http://%s/", dstIP))
	return code == 0
}

// canReach polls wget from srcPod to dstIP:80 and returns true if any attempt
// within timeout succeeds. Used to assert allow.
func canReach(t *testing.T, ns, srcPod, dstIP string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if wget(t, ns, srcPod, dstIP) {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// waitForLabelGone polls the pod until the named label is absent, or fails
// after timeout. Gate cannotReach on it after a relabel: cannotReach fails on
// the first success, so it must not run before the operator has stripped the
// system label the membership policy selects on.
func waitForLabelGone(t *testing.T, ns, pod, labelKey string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, code := kubectl(t, "get", "pod", "-n", ns, pod, "-o", "jsonpath={.metadata.labels}")
		if code == 0 && !strings.Contains(out, labelKey) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for label %s to be removed from %s/%s", labelKey, ns, pod)
}

// cannotReach returns true if every wget attempt within timeout fails. Used to
// assert deny. After a state change, gate it first (see waitForLabelGone).
func cannotReach(t *testing.T, ns, srcPod, dstIP string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if wget(t, ns, srcPod, dstIP) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
	return true
}

// uniqueNS returns a randomized e2e namespace name.
func uniqueNS(t *testing.T, prefix string) string {
	t.Helper()
	return "e2e-" + prefix + "-" + rand.String(5)
}
