//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// kubectl runs `kubectl ARGS...` and returns combined stdout+stderr and exit code.
func kubectl(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil && exit == 0 {
		exit = -1
	}
	return buf.String(), exit
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

// deleteYAML pipes the given YAML to `kubectl delete -f - --ignore-not-found`.
func deleteYAML(t *testing.T, yaml string) {
	t.Helper()
	cmd := exec.Command("kubectl", "delete", "-f", "-", "--ignore-not-found", "--wait=false")
	cmd.Stdin = strings.NewReader(yaml)
	_ = cmd.Run()
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

// canReach polls `wget` from `srcPod` to `dstIP:80` and returns true if any
// attempt within `timeout` succeeds. Used to assert allow.
func canReach(t *testing.T, ns, srcPod, dstIP string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, code := kubectl(t, "exec", "-n", ns, srcPod, "--",
			"wget", "-q", "-T", "2", "-O", "-", fmt.Sprintf("http://%s/", dstIP))
		if code == 0 {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// cannotReach returns true if every wget attempt within `timeout` fails.
// Used to assert deny — needs a long enough window that we're confident
// policies have converged.
func cannotReach(t *testing.T, ns, srcPod, dstIP string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, code := kubectl(t, "exec", "-n", ns, srcPod, "--",
			"wget", "-q", "-T", "2", "-O", "-", fmt.Sprintf("http://%s/", dstIP))
		if code == 0 {
			return false
		}
		time.Sleep(2 * time.Second)
	}
	return true
}

// uniqueNS returns a randomized e2e namespace name.
func uniqueNS(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("e2e-%s-%d", prefix, time.Now().UnixNano()%100000)
}
