package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDesiredBaseline_DenyAll(t *testing.T) {
	p := DesiredBaseline("platform")
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
	// Per ADR 0035, the baseline selects every pod in the namespace —
	// empty PodSelector (no matchLabels, no matchExpressions). The previous
	// `--elide-baseline-for` mechanism that added a NotIn matchExpression
	// was removed because it had no observable effect on connectivity.
	if len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("expected empty matchExpressions, got %+v", p.Spec.PodSelector.MatchExpressions)
	}
	if len(p.Spec.PodSelector.MatchLabels) != 0 {
		t.Errorf("expected empty matchLabels, got %+v", p.Spec.PodSelector.MatchLabels)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("baseline should not restrict egress, got %+v", p.Spec.Egress)
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
