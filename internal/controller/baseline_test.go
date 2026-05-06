package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testOperatorNS = "kube-vnet-system"

func TestDesiredBaseline_DenyAllWithEmptyElide(t *testing.T) {
	p := DesiredBaseline("platform", testOperatorNS, nil)
	if p == nil {
		t.Fatalf("DesiredBaseline returned nil")
	}
	if p.Name != BaselinePolicyName || p.Namespace != "platform" {
		t.Errorf("got %s/%s", p.Namespace, p.Name)
	}
	if p.Labels[LabelManagedBy] != LabelManagedByValue || p.Labels[LabelRole] != LabelRoleBaseline {
		t.Errorf("missing labels: %v", p.Labels)
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes should be [Ingress] only, got %v", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 0 {
		t.Errorf("expected no ingress rules (deny-all), got %+v", p.Spec.Ingress)
	}
	if len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("expected empty matchExpressions with no elide, got %+v", p.Spec.PodSelector.MatchExpressions)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("baseline should not restrict egress, got %+v", p.Spec.Egress)
	}
}

func TestDesiredBaseline_ElideForCluster(t *testing.T) {
	// `cluster` is the cluster-wide singleton; per ADR 0033 (Amendment) it
	// collapses to bare regardless of input form. The baseline elide-list
	// emits the bare key kube-vnet.system/net.cluster — operatorNS not
	// embedded.
	p := DesiredBaseline("platform", testOperatorNS, []string{"cluster"})
	if len(p.Spec.Ingress) != 0 {
		t.Errorf("expected deny-all, got %+v", p.Spec.Ingress)
	}
	if len(p.Spec.PodSelector.MatchExpressions) != 1 {
		t.Fatalf("expected one matchExpression, got %d: %+v",
			len(p.Spec.PodSelector.MatchExpressions), p.Spec.PodSelector.MatchExpressions)
	}
	expr := p.Spec.PodSelector.MatchExpressions[0]
	wantKey := "kube-vnet.system/net.cluster"
	if expr.Key != wantKey {
		t.Errorf("expr key = %q, want %q", expr.Key, wantKey)
	}
	if expr.Operator != metav1.LabelSelectorOpNotIn {
		t.Errorf("expr operator = %v, want NotIn", expr.Operator)
	}
	wantValues := []string{"both", "ingress"}
	if len(expr.Values) != len(wantValues) {
		t.Errorf("expr values = %v, want %v", expr.Values, wantValues)
	}
}

func TestDesiredBaseline_ElideForNamespace_PerNS(t *testing.T) {
	// Bare `namespace` follows the bare → `<thisNS>.<suffix>` rule.
	// Different rendering namespaces must produce different keys.
	cases := []struct {
		ns      string
		wantKey string
	}{
		{"platform", "kube-vnet.system/net.platform.namespace"},
		{"webapp", "kube-vnet.system/net.webapp.namespace"},
	}
	for _, tc := range cases {
		p := DesiredBaseline(tc.ns, testOperatorNS, []string{"namespace"})
		if len(p.Spec.PodSelector.MatchExpressions) != 1 {
			t.Fatalf("ns=%s: expected one matchExpression, got %d", tc.ns, len(p.Spec.PodSelector.MatchExpressions))
		}
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != tc.wantKey {
			t.Errorf("ns=%s: key = %q, want %q", tc.ns, got, tc.wantKey)
		}
	}
}

func TestDesiredBaseline_ElideForBareUserVnet_PerNS(t *testing.T) {
	// A bare user-vnet name follows the same rule as `namespace`: per-NS render.
	p := DesiredBaseline("platform", testOperatorNS, []string{"foo"})
	if len(p.Spec.PodSelector.MatchExpressions) != 1 {
		t.Fatalf("expected one matchExpression")
	}
	wantKey := "kube-vnet.system/net.platform.foo"
	if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != wantKey {
		t.Errorf("key = %q, want %q", got, wantKey)
	}
}

func TestDesiredBaseline_ElideForFQ_PassThrough(t *testing.T) {
	// Already-FQ entries pass through. Includes `<homeNS>.namespace` (a
	// specific NS's namespace vnet) and `<homeNS>.<vnet>` (a specific user vnet).
	p := DesiredBaseline("platform", testOperatorNS, []string{"payments.namespace", "monitoring.observability"})
	if len(p.Spec.PodSelector.MatchExpressions) != 2 {
		t.Fatalf("expected two matchExpressions, got %d", len(p.Spec.PodSelector.MatchExpressions))
	}
	wantKeys := map[string]bool{
		"kube-vnet.system/net.payments.namespace":       true,
		"kube-vnet.system/net.monitoring.observability": true,
	}
	for _, expr := range p.Spec.PodSelector.MatchExpressions {
		if !wantKeys[expr.Key] {
			t.Errorf("unexpected key %q", expr.Key)
		}
	}
}

func TestDesiredBaseline_ElideMultipleVnets(t *testing.T) {
	p := DesiredBaseline("platform", testOperatorNS, []string{"cluster", "namespace"})
	if len(p.Spec.PodSelector.MatchExpressions) != 2 {
		t.Errorf("expected two matchExpressions, got %d", len(p.Spec.PodSelector.MatchExpressions))
	}
}

func TestNamespaceFilter(t *testing.T) {
	f := NewNamespaceFilter([]string{"kube-system", "kube-public"})
	if f.IsManagedName("kube-system") {
		t.Errorf("kube-system should be excluded")
	}
	if !f.IsManagedName("platform") {
		t.Errorf("platform should be managed")
	}
	if !f.IsManaged(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform"}}) {
		t.Errorf("plain platform should be managed")
	}
	disabled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "platform",
		Annotations: map[string]string{AnnotationDisabled: "true"},
	}}
	if f.IsManaged(disabled) {
		t.Errorf("annotated platform should be unmanaged")
	}
}
