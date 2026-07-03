package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestAllBuilders_StampStandardManagedByLabel asserts every operator-emitted
// resource carries the Kubernetes recommended `app.kubernetes.io/managed-by`
// label (informational, for dashboards and by-convention kubectl queries)
// in ADDITION to the authoritative `kube-vnet.system/managed-by`. See the
// LabelK8sManagedBy doc comment for why the standard label must never
// replace the system one.
func TestAllBuilders_StampStandardManagedByLabel(t *testing.T) {
	check := func(t *testing.T, what string, labels map[string]string) {
		t.Helper()
		if labels[LabelK8sManagedBy] != LabelManagedByValue {
			t.Errorf("%s: missing %s=%s (got %q)", what, LabelK8sManagedBy, LabelManagedByValue, labels[LabelK8sManagedBy])
		}
		if labels[LabelManagedBy] != LabelManagedByValue {
			t.Errorf("%s: the authoritative system label must remain present alongside the standard one", what)
		}
	}

	// 1. Baseline.
	check(t, "baseline", DesiredBaseline("ns").Labels)

	// 2. Membership policy.
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform"),
		MembersByNS: map[string]map[Direction][]string{
			"platform": {DirectionBoth: {"p1"}},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 membership policy, got %d", len(out.Policies))
	}
	check(t, "membership", out.Policies[0].Labels)

	// 3. External-allow (Service source).
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80)}},
		},
	}
	extPol, err := buildExternalAllowPolicy(lb, nil)
	if err != nil || extPol == nil {
		t.Fatalf("buildExternalAllowPolicy: pol=%v err=%v", extPol, err)
	}
	check(t, "ext.svc", extPol.Labels)

	// 4. External-allow (host source).
	check(t, "ext.host", buildHostPortPolicy("ns", hostPortKey{port: 8080, protocol: corev1.ProtocolTCP}).Labels)

	// 5. External-allow (apiserver source).
	webhookSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "webhook"},
			Ports:    []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(8443)}},
		},
	}
	apiPol, err := buildApiserverReachablePolicy(webhookSvc, nil, []int32{443}, "0.0.0.0/0")
	if err != nil {
		t.Fatalf("buildApiserverReachablePolicy: %v", err)
	}
	check(t, "ext.apiserver", apiPol.Labels)

	// 6. System vnets.
	check(t, "system-vnet", desiredSystemVnet(SystemVnetNamespace, "ns", "test").Labels)
}
