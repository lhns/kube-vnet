package controller

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Every reconciler emits Events through mgr.GetEventRecorder, which is
// controller-runtime's NEW events API: its sink is
// events.EventSinkImpl{Interface: eventsv1client…}, so writes go to
// events.k8s.io/v1 — NOT the core "" group.
//
// The core group is still required, because controller-runtime's leader
// election uses the deprecated core-v1 recorder. Granting only one of the two
// means either every operator Event or every leader-election Event is
// forbidden, and the operator log fills with RBAC errors while the user-facing
// diagnostics (VirtualNetworkNotJoinable, InvalidJoinLabelDirection, Ready,
// Degraded, PolicyRestored) silently never appear.
//
// envtest does not enforce RBAC, so no envtest-based test can catch this —
// these manifest assertions are the only guard. Both the kustomize role and the
// chart's ClusterRole are checked, since they are maintained separately and can
// drift apart.
var eventsRequiredGroups = []string{"", "events.k8s.io"}

// grantsEvents reports whether the rules create+patch Events in apiGroup.
func grantsEvents(rules []rbacv1.PolicyRule, apiGroup string) bool {
	hasAll := func(have []string, want ...string) bool {
		for _, w := range want {
			found := false
			for _, h := range have {
				if h == w || h == "*" {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	for _, r := range rules {
		if hasAll(r.APIGroups, apiGroup) && hasAll(r.Resources, "events") &&
			hasAll(r.Verbs, "create", "patch") {
			return true
		}
	}
	return false
}

func TestRBAC_KustomizeRoleGrantsEventsInBothGroups(t *testing.T) {
	path := filepath.Join("..", "..", "config", "rbac", "role.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(&role); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	for _, g := range eventsRequiredGroups {
		if !grantsEvents(role.Rules, g) {
			t.Errorf("config/rbac/role.yaml does not grant events create+patch in apiGroup %q; "+
				"regenerate it from the kubebuilder markers (controller-gen rbac:...)", g)
		}
	}
}

func TestRBAC_ChartClusterRoleGrantsEventsInBothGroups(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart RBAC render")
	}
	chartDir := filepath.Join("..", "..", "charts", "kube-vnet")
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("helm", "template", "rbacevents", chartDir,
		"--kube-version", "1.31.0",
		"--show-only", "templates/clusterrole.yaml",
		// The chart deliberately refuses to default this; see cluster-baseline.yaml.
		"--set", "operator.clusterBaseline.ingressIsolationLevel=namespace",
	)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\nstderr: %s", err, stderr.String())
	}

	var rules []rbacv1.PolicyRule
	dec := yaml.NewYAMLOrJSONDecoder(&stdout, 4096)
	for {
		var role rbacv1.ClusterRole
		if err := dec.Decode(&role); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered ClusterRole: %v", err)
		}
		rules = append(rules, role.Rules...)
	}
	if len(rules) == 0 {
		t.Fatal("chart rendered no ClusterRole rules")
	}
	for _, g := range eventsRequiredGroups {
		if !grantsEvents(rules, g) {
			t.Errorf("chart ClusterRole does not grant events create+patch in apiGroup %q", g)
		}
	}
}
