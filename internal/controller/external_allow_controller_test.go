package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// svc returns a minimal Service skeleton; specific fields overridden per-test.
func svc(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func TestBuildExternalAllowPolicy_LoadBalancer_NumericPort(t *testing.T) {
	s := svc("traefik", "traefik")
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol == nil {
		t.Fatal("expected policy, got nil")
	}
	if pol.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("missing managed-by label: %v", pol.Labels)
	}
	if pol.Labels[LabelRole] != LabelRoleExternalAllow {
		t.Errorf("wrong role label: %q", pol.Labels[LabelRole])
	}
	if pol.Labels[LabelSource] != "svc-traefik" {
		t.Errorf("wrong source label: %q", pol.Labels[LabelSource])
	}
	if pol.Labels[LabelSourceKind] != LabelSourceKindService {
		t.Errorf("wrong source-kind label: %q", pol.Labels[LabelSourceKind])
	}
	if got := pol.Spec.PodSelector.MatchLabels["app"]; got != "traefik" {
		t.Errorf("podSelector.matchLabels[app] = %q, want traefik", got)
	}
	if len(pol.Spec.Ingress) != 1 || len(pol.Spec.Ingress[0].From) != 1 ||
		pol.Spec.Ingress[0].From[0].IPBlock == nil {
		t.Fatalf("expected one from-rule with ipBlock, got %+v", pol.Spec.Ingress)
	}
	if pol.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("ipBlock cidr = %q, want 0.0.0.0/0", pol.Spec.Ingress[0].From[0].IPBlock.CIDR)
	}
	if len(pol.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("expected one port, got %d", len(pol.Spec.Ingress[0].Ports))
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 80 {
		t.Errorf("port = %d, want 80", got)
	}
	if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", pol.Spec.PolicyTypes)
	}
}

func TestBuildExternalAllowPolicy_NodePort_TargetPortToPodSide(t *testing.T) {
	// Allowed port must be the pod-side targetPort, NOT the Service Port nor
	// the nodePort. By the time external traffic reaches the pod, kube-proxy
	// has DNAT'd node:nodePort → pod:targetPort.
	s := svc("api", "api")
	s.Spec.Type = corev1.ServiceTypeNodePort
	s.Spec.Ports = []corev1.ServicePort{{
		Port:       80,
		TargetPort: intstr.FromInt32(8080),
		NodePort:   32100,
		Protocol:   corev1.ProtocolTCP,
	}}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8080 {
		t.Errorf("port = %d, want 8080 (targetPort, not 80=port or 32100=nodePort)", got)
	}
}

func TestBuildExternalAllowPolicy_NodePort_NamedPort_PodPresent(t *testing.T) {
	s := svc("api", "api")
	s.Spec.Type = corev1.ServiceTypeNodePort
	s.Spec.Ports = []corev1.ServicePort{{
		Port: 80, TargetPort: intstr.FromString("http-port"),
	}}
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Ports: []corev1.ContainerPort{{Name: "http-port", ContainerPort: 8443}},
			}},
		},
	}}
	pol, err := buildExternalAllowPolicy(s, pods)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8443 {
		t.Errorf("named port resolved to %d, want 8443", got)
	}
}

func TestBuildExternalAllowPolicy_NodePort_NamedPort_NoPodYet(t *testing.T) {
	s := svc("api", "api")
	s.Spec.Ports = []corev1.ServicePort{{
		Port: 80, TargetPort: intstr.FromString("http"),
	}}
	_, err := buildExternalAllowPolicy(s, nil)
	if !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("err = %v, want errNamedPortUnresolvable", err)
	}
}

func TestBuildExternalAllowPolicy_ClusterIP_WithExternalIPs(t *testing.T) {
	s := svc("admin", "ops")
	s.Spec.Type = corev1.ServiceTypeClusterIP
	s.Spec.ExternalIPs = []string{"10.0.0.1"}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol == nil {
		t.Fatal("ClusterIP+externalIPs should emit a policy")
	}
}

func TestBuildExternalAllowPolicy_ClusterIP_NoExternalIPs(t *testing.T) {
	s := svc("internal", "app")
	s.Spec.Type = corev1.ServiceTypeClusterIP
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol != nil {
		t.Errorf("plain ClusterIP shouldn't emit a policy, got: %v", pol.Name)
	}
}

func TestBuildExternalAllowPolicy_Headless(t *testing.T) {
	s := svc("headless", "app")
	s.Spec.ClusterIP = corev1.ClusterIPNone
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil || pol != nil {
		t.Errorf("headless should yield (nil, nil), got (%v, %v)", pol, err)
	}
}

func TestBuildExternalAllowPolicy_ExternalName(t *testing.T) {
	s := svc("dns-alias", "app")
	s.Spec.Type = corev1.ServiceTypeExternalName
	s.Spec.ExternalName = "elsewhere.example.com"
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil || pol != nil {
		t.Errorf("ExternalName should yield (nil, nil), got (%v, %v)", pol, err)
	}
}

func TestBuildExternalAllowPolicy_NilSelector(t *testing.T) {
	s := svc("manual-endpoints", "app")
	s.Spec.Selector = nil
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil || pol != nil {
		t.Errorf("nil-selector Service should yield (nil, nil), got (%v, %v)", pol, err)
	}
}

func TestBuildExternalAllowPolicy_MultiPort(t *testing.T) {
	s := svc("multi", "app")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)},
		{Name: "https", Port: 443, TargetPort: intstr.FromInt32(443)},
	}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := len(pol.Spec.Ingress[0].Ports); got != 2 {
		t.Errorf("expected 2 ports, got %d", got)
	}
}

func TestBuildExternalAllowPolicy_MultiPort_OneUnresolvableNamedPort(t *testing.T) {
	// Partial emission would create a confusing in-between state; one
	// unresolvable port triggers full requeue.
	s := svc("multi", "app")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)},
		{Name: "metrics", Port: 9100, TargetPort: intstr.FromString("metrics")},
	}
	_, err := buildExternalAllowPolicy(s, nil)
	if !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("err = %v, want errNamedPortUnresolvable", err)
	}
}

func TestBuildExternalAllowPolicy_TargetPortUnset_DefaultsToPort(t *testing.T) {
	s := svc("default-target", "app")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 8080}, // TargetPort omitted
	}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8080 {
		t.Errorf("unset targetPort should default to Port (8080), got %d", got)
	}
}

func TestExternalAllowPolicyName_DeterministicAndCapped(t *testing.T) {
	long := "this-is-a-very-long-service-name-that-exceeds-some-limit"
	s := svc(long, "ns")
	name := externalAllowPolicyName(s)
	if len(name) > 63 {
		t.Errorf("name length %d > 63: %q", len(name), name)
	}
	if !strings.HasPrefix(name, "kube-vnet.ext.svc.") {
		t.Errorf("missing prefix: %q", name)
	}
	// Determinism.
	if name != externalAllowPolicyName(s) {
		t.Error("name is non-deterministic")
	}
}

func TestExternalAllowPolicyName_DistinctSvcsDifferentHash(t *testing.T) {
	// Two different long names that share a truncated prefix must still
	// produce distinct policy names.
	a := svc("very-long-name-aaaaaaaaaaaaaaaaaaaaaaaaaaa", "ns")
	b := svc("very-long-name-aaaaaaaaaaaaaaaaaaaaaaaaaaa-2", "ns")
	if externalAllowPolicyName(a) == externalAllowPolicyName(b) {
		t.Errorf("name collision: %q == %q", externalAllowPolicyName(a), externalAllowPolicyName(b))
	}
}

func TestExternalAllowOptedOut_AnnotationValueParsing(t *testing.T) {
	cases := map[string]bool{
		"false":   true,
		"true":    false,
		"":        false,
		"FALSE":   false, // case-sensitive on purpose: only exact "false"
		"no":      false,
		"0":       false,
		"yes":     false,
		"disable": false,
	}
	for v, wantOptOut := range cases {
		t.Run("v="+v, func(t *testing.T) {
			got := ExternalAllowOptedOut(map[string]string{AnnotationExternalAllow: v})
			if got != wantOptOut {
				t.Errorf("value %q: opted-out=%v, want %v", v, got, wantOptOut)
			}
		})
	}
	if ExternalAllowOptedOut(nil) {
		t.Error("nil annotations should not be opted-out")
	}
	if ExternalAllowOptedOut(map[string]string{}) {
		t.Error("empty annotations should not be opted-out")
	}
}

func TestIsExternallyExposed_TypeTable(t *testing.T) {
	cases := []struct {
		name string
		svc  corev1.Service
		want bool
	}{
		{"LB", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}, true},
		{"NodePort", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}}, true},
		{"ClusterIP+externalIPs", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ExternalIPs: []string{"1.2.3.4"}}}, true},
		{"ClusterIP_plain", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}, false},
		{"ExternalName", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName}}, false},
		{"Headless", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, ClusterIP: corev1.ClusterIPNone}}, false},
		{"empty_type_with_externalIPs", corev1.Service{Spec: corev1.ServiceSpec{ExternalIPs: []string{"1.2.3.4"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isExternallyExposed(&c.svc); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestExternalAllowPolicyToService_DerivesFromLabel(t *testing.T) {
	// Mapper extracts the source Service name from a `svc-<name>`
	// LabelSource value when LabelSourceKind=svc. Host-source policies
	// don't enqueue Service reconciles; neither do un-labeled junk.
	cases := []struct {
		name        string
		kind        string
		src         string
		expectName  string
		expectEmpty bool
	}{
		{name: "service_source", kind: LabelSourceKindService, src: "svc-traefik", expectName: "traefik"},
		{name: "host_source_skipped", kind: LabelSourceKindHost, src: "host-8080-tcp", expectEmpty: true},
		{name: "no_kind_label", kind: "", src: "svc-traefik", expectEmpty: true},
		{name: "missing_svc_prefix", kind: LabelSourceKindService, src: "traefik", expectEmpty: true},
		{name: "empty_src", kind: LabelSourceKindService, src: "svc-", expectEmpty: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "fake",
					Labels: map[string]string{
						LabelSourceKind: c.kind,
						LabelSource:     c.src,
					},
				},
			}
			reqs := externalAllowPolicyToService(context.Background(), obj)
			if c.expectEmpty {
				if len(reqs) != 0 {
					t.Errorf("expected empty, got %v", reqs)
				}
				return
			}
			if len(reqs) != 1 || reqs[0].Name != c.expectName {
				t.Errorf("got %v, want one request for %q", reqs, c.expectName)
			}
		})
	}
}
