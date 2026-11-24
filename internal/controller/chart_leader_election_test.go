package controller

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChart_LeaderElectionTimingFlags(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render")
	}
	render := func(t *testing.T, sets ...string) string {
		t.Helper()
		args := []string{
			"template", "leflags", filepath.Join("..", "..", "charts", "kube-vnet"),
			"--kube-version", "1.31.0",
			"--show-only", "templates/deployment.yaml",
			"--set", "operator.clusterBaseline.ingressIsolationLevel=pod",
		}
		for _, s := range sets {
			args = append(args, "--set", s)
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("helm", args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("helm template: %v\nstderr: %s", err, stderr.String())
		}
		return strings.ReplaceAll(stdout.String(), "\r\n", "\n")
	}
	const prefix = "--leader-election-"
	all := []string{
		"operator.leaderElection.leaseDuration=60s",
		"operator.leaderElection.renewDeadline=40s",
		"operator.leaderElection.retryPeriod=5s",
	}

	t.Run("unset renders no timing flag", func(t *testing.T) {
		out := render(t)
		if !strings.Contains(out, "--leader-elect\n") {
			t.Error("--leader-elect missing")
		}
		if strings.Contains(out, prefix) {
			t.Errorf("unexpected %s* flag with values unset:\n%s", prefix, out)
		}
	})
	t.Run("set renders each flag", func(t *testing.T) {
		out := render(t, all...)
		for _, want := range []string{
			"--leader-election-lease-duration=60s",
			"--leader-election-renew-deadline=40s",
			"--leader-election-retry-period=5s",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %s", want)
			}
		}
	})
	t.Run("only the set flag renders", func(t *testing.T) {
		out := render(t, "operator.leaderElection.renewDeadline=40s")
		if !strings.Contains(out, "--leader-election-renew-deadline=40s") {
			t.Error("renew-deadline missing")
		}
		if strings.Contains(out, "lease-duration") || strings.Contains(out, "retry-period") {
			t.Errorf("unset flags rendered:\n%s", out)
		}
	})
	t.Run("leaderElect=false renders none", func(t *testing.T) {
		out := render(t, append([]string{"operator.leaderElect=false"}, all...)...)
		if strings.Contains(out, prefix) || strings.Contains(out, "--leader-elect") {
			t.Errorf("leader-election flags rendered with leaderElect=false:\n%s", out)
		}
	})
}
