package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDesiredBaseline(t *testing.T) {
	p := DesiredBaseline("platform")
	if p.Name != BaselinePolicyName || p.Namespace != "platform" {
		t.Fatalf("got %s/%s", p.Namespace, p.Name)
	}
	if p.Labels[LabelManagedBy] != LabelManagedByValue || p.Labels[LabelRole] != LabelRoleBaseline {
		t.Errorf("missing labels: %v", p.Labels)
	}
	// Empty podSelector applies to all pods.
	if len(p.Spec.PodSelector.MatchLabels) != 0 || len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("podSelector not empty")
	}
	// Both Ingress and Egress in PolicyTypes (so unmatched traffic is dropped).
	hasIngress, hasEgress := false, false
	for _, t := range p.Spec.PolicyTypes {
		if t == networkingv1.PolicyTypeIngress {
			hasIngress = true
		}
		if t == networkingv1.PolicyTypeEgress {
			hasEgress = true
		}
	}
	if !hasIngress || !hasEgress {
		t.Errorf("policyTypes incomplete: %v", p.Spec.PolicyTypes)
	}
	// CoreDNS allowance.
	if len(p.Spec.Egress) != 1 || len(p.Spec.Egress[0].Ports) != 2 {
		t.Errorf("expected one DNS egress rule with 2 ports")
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
