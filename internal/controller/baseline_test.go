package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDesiredBaseline_None_AllowAll(t *testing.T) {
	p := DesiredBaseline("platform", IsolationNone)
	if p == nil {
		t.Fatalf("IsolationNone should return an allow-all baseline, got nil")
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
	// Allow-all is expressed as one empty ingress rule (no `from`, no `ports`),
	// per K8s NetworkPolicy semantics. NetworkPolicyPeer can't itself be empty.
	if len(p.Spec.Ingress) != 1 {
		t.Fatalf("expected one ingress rule, got %d: %+v", len(p.Spec.Ingress), p.Spec.Ingress)
	}
	rule := p.Spec.Ingress[0]
	if len(rule.From) != 0 || len(rule.Ports) != 0 {
		t.Errorf("allow-all rule must have empty From and Ports, got %+v", rule)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("baseline should not restrict egress, got %+v", p.Spec.Egress)
	}
}

func TestDesiredBaseline_Pod(t *testing.T) {
	p := DesiredBaseline("platform", IsolationPod)
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
	// Ingress only — egress is unrestricted under the new model (ADR 0025).
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes should be [Ingress] only, got %v", p.Spec.PolicyTypes)
	}
	if p.Spec.Ingress != nil {
		t.Errorf("IsolationPod should have nil ingress (deny-all), got %+v", p.Spec.Ingress)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("baseline should not restrict egress, got %+v", p.Spec.Egress)
	}
}

func TestDesiredBaseline_Namespace(t *testing.T) {
	p := DesiredBaseline("platform", IsolationNamespace)
	if p.Name != BaselinePolicyName || p.Namespace != "platform" {
		t.Fatalf("got %s/%s", p.Namespace, p.Name)
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes should be [Ingress] only, got %v", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 1 {
		t.Fatalf("expected one ingress rule with one peer, got %+v", p.Spec.Ingress)
	}
	from := p.Spec.Ingress[0].From[0]
	if from.NamespaceSelector == nil ||
		from.NamespaceSelector.MatchLabels[NamespaceMetadataNameLabel] != "platform" {
		t.Errorf("ingress peer should select same namespace, got %+v", from)
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

func TestNamespaceFilter_ResolveIsolation(t *testing.T) {
	f := NewNamespaceFilter(nil)
	f.DefaultIsolation = IsolationNamespace
	f.OverrideIsolationPod["secured"] = true
	f.OverrideIsolationNone["dev"] = true

	// Default applies when no annotation, no override.
	if got := f.ResolveIsolation(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform"}}); got != IsolationNamespace {
		t.Errorf("default = %v, want %v", got, IsolationNamespace)
	}
	// Override list wins over default.
	if got := f.ResolveIsolation(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "secured"}}); got != IsolationPod {
		t.Errorf("override pod = %v, want %v", got, IsolationPod)
	}
	if got := f.ResolveIsolation(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dev"}}); got != IsolationNone {
		t.Errorf("override none = %v, want %v", got, IsolationNone)
	}
	// Annotation wins over override.
	annotated := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "secured",
		Annotations: map[string]string{AnnotationIngressIsolation: "namespace"},
	}}
	if got := f.ResolveIsolation(annotated); got != IsolationNamespace {
		t.Errorf("annotation = %v, want %v", got, IsolationNamespace)
	}
}
