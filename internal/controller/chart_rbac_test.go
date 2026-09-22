//go:build integration

package controller

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestChartRBAC_MatchesKubebuilder asserts the chart's ClusterRole rules
// equal the kubebuilder-generated config/rbac/role.yaml after normalization.
//
// The chart's ClusterRole is hand-written, because controller-gen can't emit
// Helm-templated names. When it once lacked pods/patch and
// virtualnetworks/create nothing noticed: every other test path runs as an
// envtest admin or installs config/default, which uses the generated rules.
// See ADR 0030.
func TestChartRBAC_MatchesKubebuilder(t *testing.T) {
	// ingressIsolationLevel has no default (ADR 0031); any valid value renders.
	rendered := helmTemplate(t, "testrelease", "--set", "operator.clusterBaseline.ingressIsolationLevel=namespace")
	chartRules := extractClusterRoleRules(t, bytes.NewReader(rendered))

	roleYAML, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("read config/rbac/role.yaml: %v", err)
	}
	kubebuilderRules := extractClusterRoleRules(t, bytes.NewReader(roleYAML))

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

// extractClusterRoleRules collects the rules of every ClusterRole in a YAML
// stream, except the ancillary ones config/rbac/role.yaml doesn't mirror.
func extractClusterRoleRules(t *testing.T, in io.Reader) []rbacv1.PolicyRule {
	t.Helper()
	var rules []rbacv1.PolicyRule
	for _, obj := range decodeObjects(t, in) {
		if obj.GetKind() != "ClusterRole" {
			continue
		}
		var cr rbacv1.ClusterRole
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &cr); err != nil {
			t.Fatalf("convert ClusterRole %s: %v", obj.GetName(), err)
		}
		if isAncillaryClusterRole(&cr) {
			continue
		}
		rules = append(rules, cr.Rules...)
	}
	return rules
}

// isAncillaryClusterRole reports chart ClusterRoles that are not the
// operator's own permissions:
//
//  1. End-user roles (ADR 0031): an `rbac.authorization.k8s.io/aggregate-to-*`
//     label, or the `-editor`/`-viewer` suffix (the cluster-baseline pair has
//     no aggregation labels).
//  2. Helm hook roles (ADR 0036 pre-delete cleanup): the `helm.sh/hook`
//     annotation. They live only for the hook run.
func isAncillaryClusterRole(cr *rbacv1.ClusterRole) bool {
	for k := range cr.Labels {
		if strings.HasPrefix(k, "rbac.authorization.k8s.io/aggregate-to-") {
			return true
		}
	}
	if strings.HasSuffix(cr.Name, "-editor") || strings.HasSuffix(cr.Name, "-viewer") {
		return true
	}
	_, hook := cr.Annotations["helm.sh/hook"]
	return hook
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
