package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDesiredBaseline_DenyAllWithEmptyElide(t *testing.T) {
	p := DesiredBaseline("platform", IsolationNone, nil)
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
	// Deny-all baseline: no ingress rules.
	if len(p.Spec.Ingress) != 0 {
		t.Errorf("expected no ingress rules (deny-all), got %+v", p.Spec.Ingress)
	}
	// Empty elide list → empty matchExpressions on the podSelector,
	// meaning the baseline selects every pod.
	if len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("expected empty matchExpressions with no elide, got %+v", p.Spec.PodSelector.MatchExpressions)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("baseline should not restrict egress, got %+v", p.Spec.Egress)
	}
}

func TestDesiredBaseline_ElideForCluster(t *testing.T) {
	p := DesiredBaseline("platform", IsolationNone, []string{"cluster"})
	// Deny-all + one matchExpression excluding cluster receivers.
	if len(p.Spec.Ingress) != 0 {
		t.Errorf("expected deny-all, got %+v", p.Spec.Ingress)
	}
	if len(p.Spec.PodSelector.MatchExpressions) != 1 {
		t.Fatalf("expected one matchExpression, got %d: %+v",
			len(p.Spec.PodSelector.MatchExpressions), p.Spec.PodSelector.MatchExpressions)
	}
	expr := p.Spec.PodSelector.MatchExpressions[0]
	if expr.Key != "kube-vnet.system/net.cluster" {
		t.Errorf("expr key = %q, want kube-vnet.system/net.cluster", expr.Key)
	}
	if expr.Operator != metav1.LabelSelectorOpNotIn {
		t.Errorf("expr operator = %v, want NotIn", expr.Operator)
	}
	wantValues := []string{"both", "ingress"}
	if len(expr.Values) != len(wantValues) {
		t.Errorf("expr values = %v, want %v", expr.Values, wantValues)
	}
}

func TestDesiredBaseline_ElideMultipleVnets(t *testing.T) {
	p := DesiredBaseline("platform", IsolationNone, []string{"cluster", "public"})
	if len(p.Spec.PodSelector.MatchExpressions) != 2 {
		t.Errorf("expected two matchExpressions, got %d", len(p.Spec.PodSelector.MatchExpressions))
	}
}

func TestParseIsolationMode(t *testing.T) {
	type tc struct {
		in   string
		want IsolationMode
		ok   bool
	}
	cases := []tc{
		{"none", IsolationNone, true},
		{"", IsolationNone, true},
		{"namespace", IsolationNamespace, true},
		{"pod", IsolationPod, true},
		{"strict", IsolationNone, false},
		{"NONE", IsolationNone, false}, // case-sensitive
	}
	for _, c := range cases {
		got, ok := ParseIsolationMode(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseIsolationMode(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
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
