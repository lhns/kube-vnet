//go:build integration

package controller

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// TestChartRBAC_MatchesKubebuilder asserts the chart's ClusterRole rules
// equal the kubebuilder-generated config/rbac/role.yaml after normalization.
//
// Why this test exists: the chart's ClusterRole is hand-written (it has to
// be, because Helm-templated names like {{ include "fullname" . }} aren't
// something controller-gen can emit). Stage A's bug — chart RBAC missing
// pods/patch and virtualnetworks/create — went undetected because every
// other test path runs against envtest as a system admin or installs via
// kubectl apply -k config/default (which uses the kubebuilder-generated
// rules). Real Helm installs never had their RBAC checked.
//
// The fix: compare the rules verb-for-verb every CI run. If a +kubebuilder:rbac
// annotation changes (or someone hand-edits the chart template), this test
// fails with a precise diff and the contributor knows to update the other
// side.
//
// See ADR 0030 stage-A bug fix and the post-audit cleanup plan.
func TestChartRBAC_MatchesKubebuilder(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart-RBAC drift test")
	}

	// Render the chart's ClusterRole.
	rendered, err := renderChart(t)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	chartRules := extractClusterRoleRules(t, bytes.NewReader(rendered))

	// Read and parse config/rbac/role.yaml.
	roleYAML, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("read config/rbac/role.yaml: %v", err)
	}
	kubebuilderRules := extractClusterRoleRules(t, bytes.NewReader(roleYAML))

	// Normalize both: each rule's apiGroups/resources/verbs sorted; rules
	// themselves sorted by (first apiGroup, first resource, first verb).
	normalizeRules(chartRules)
	normalizeRules(kubebuilderRules)

	if !reflect.DeepEqual(chartRules, kubebuilderRules) {
		t.Errorf("chart ClusterRole rules differ from config/rbac/role.yaml.\n"+
			"chart:        %s\nkubebuilder: %s\n"+
			"To fix: regenerate config/rbac/role.yaml via `make manifests` and "+
			"hand-mirror any changes in charts/kube-vnet/templates/clusterrole.yaml. "+
			"Both files must list the same rules verb-for-verb.",
			rulesAsString(chartRules), rulesAsString(kubebuilderRules))
	}
}

// renderChart runs `helm template` with the same args as the chart-manifests
// dry-run test, returning the raw multi-document YAML.
func renderChart(t *testing.T) ([]byte, error) {
	t.Helper()
	chartDir := filepath.Join("..", "..", "charts", "kube-vnet")
	var stdout, stderr bytes.Buffer
	// operator.clusterBaseline.ingressIsolationLevel has no default per ADR
	// 0031; pass any valid value so the chart renders without erroring.
	cmd := exec.Command("helm", "template", "testrelease", chartDir,
		"--kube-version", "1.31.0",
		"--set", "operator.clusterBaseline.ingressIsolationLevel=namespace")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New(stderr.String())
	}
	return stdout.Bytes(), nil
}

// extractClusterRoleRules pulls every PolicyRule from every ClusterRole
// document in a multi-document YAML stream. Aggregated end-user
// ClusterRoles (ADR 0031 cleanup) are filtered out — they're identified by
// any `rbac.authorization.k8s.io/aggregate-to-*` label OR by the
// `-{editor,viewer}` name suffix that the chart uses for the unbound
// cluster-baseline pair (which has no aggregation labels). Only the
// operator's own ClusterRole, which `config/rbac/role.yaml` mirrors,
// participates in this drift check.
func extractClusterRoleRules(t *testing.T, in io.Reader) []rbacv1.PolicyRule {
	t.Helper()
	var rules []rbacv1.PolicyRule
	dec := yaml.NewYAMLOrJSONDecoder(in, 4096)
	for {
		var raw map[string]interface{}
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return rules
			}
			t.Fatalf("decode YAML: %v", err)
		}
		if raw["kind"] != "ClusterRole" {
			continue
		}
		// Re-encode this document and decode into rbacv1.ClusterRole — the
		// safest path to PolicyRule structs without writing a YAML→Go bridge.
		buf, err := sigsyaml.Marshal(raw)
		if err != nil {
			t.Fatalf("re-encode ClusterRole: %v", err)
		}
		var cr rbacv1.ClusterRole
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buf), 4096).Decode(&cr); err != nil {
			t.Fatalf("decode ClusterRole: %v", err)
		}
		if isEndUserAggregatedRole(&cr) {
			continue
		}
		rules = append(rules, cr.Rules...)
	}
}

// isEndUserAggregatedRole returns true for the chart's end-user-facing
// ClusterRoles (Stage A/B of the ADR-0031-RBAC PR). They are not part of
// the operator's own permissions and don't appear in config/rbac/role.yaml.
func isEndUserAggregatedRole(cr *rbacv1.ClusterRole) bool {
	for k := range cr.Labels {
		if strings.HasPrefix(k, "rbac.authorization.k8s.io/aggregate-to-") {
			return true
		}
	}
	if strings.HasSuffix(cr.Name, "-editor") || strings.HasSuffix(cr.Name, "-viewer") {
		return true
	}
	return false
}

// normalizeRules canonicalizes each rule (sort apiGroups/resources/verbs)
// then sorts the rule slice. Two semantically-equivalent rule sets become
// byte-identical after normalization.
func normalizeRules(rules []rbacv1.PolicyRule) {
	for i := range rules {
		sort.Strings(rules[i].APIGroups)
		sort.Strings(rules[i].Resources)
		sort.Strings(rules[i].Verbs)
		sort.Strings(rules[i].ResourceNames)
		sort.Strings(rules[i].NonResourceURLs)
	}
	sort.SliceStable(rules, func(i, j int) bool {
		return rulesAsString([]rbacv1.PolicyRule{rules[i]}) <
			rulesAsString([]rbacv1.PolicyRule{rules[j]})
	})
}

// rulesAsString is a deterministic stringification used both for sort keys
// and for the failure message.
func rulesAsString(rules []rbacv1.PolicyRule) string {
	var buf bytes.Buffer
	for _, r := range rules {
		buf.WriteString("[apiGroups=")
		buf.WriteString(joinSorted(r.APIGroups))
		buf.WriteString(" resources=")
		buf.WriteString(joinSorted(r.Resources))
		buf.WriteString(" verbs=")
		buf.WriteString(joinSorted(r.Verbs))
		if len(r.ResourceNames) > 0 {
			buf.WriteString(" resourceNames=")
			buf.WriteString(joinSorted(r.ResourceNames))
		}
		if len(r.NonResourceURLs) > 0 {
			buf.WriteString(" nonResourceURLs=")
			buf.WriteString(joinSorted(r.NonResourceURLs))
		}
		buf.WriteString("] ")
	}
	return buf.String()
}

func joinSorted(in []string) string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	var buf bytes.Buffer
	for i, s := range out {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(s)
	}
	return buf.String()
}
