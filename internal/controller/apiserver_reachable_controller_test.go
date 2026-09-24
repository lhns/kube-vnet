package controller

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

// ---- discovery ----

func TestServiceRefs(t *testing.T) {
	svcRef := func(port *int32) *admissionregistrationv1.ServiceReference {
		return &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "svc", Port: port}
	}
	validating := func(refs ...*admissionregistrationv1.ServiceReference) *admissionregistrationv1.ValidatingWebhookConfiguration {
		cfg := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		for _, ref := range refs {
			cc := admissionregistrationv1.WebhookClientConfig{Service: ref}
			if ref == nil {
				cc.URL = new("https://external.example.com/validate")
			}
			cfg.Webhooks = append(cfg.Webhooks, admissionregistrationv1.ValidatingWebhook{ClientConfig: cc})
		}
		return cfg
	}
	crd := func(strategy apiextensionsv1.ConversionStrategyType, cc *apiextensionsv1.WebhookClientConfig) *apiextensionsv1.CustomResourceDefinition {
		conv := &apiextensionsv1.CustomResourceConversion{Strategy: strategy}
		if cc != nil {
			conv.Webhook = &apiextensionsv1.WebhookConversion{ClientConfig: cc}
		}
		return &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{Conversion: conv}}
	}
	ref := func(port int32) serviceRef { return serviceRef{Namespace: "ns", Name: "svc", Port: port} }

	cases := []struct {
		name string
		in   runtime.Object
		want []serviceRef
	}{
		{"validating", validating(svcRef(new(int32(8443)))), []serviceRef{ref(8443)}},
		{"validating_port_defaulted_to_443", validating(svcRef(nil)), []serviceRef{ref(443)}},
		{"validating_url_only_skipped", validating(nil), nil},
		// One ref per webhook entry; the callers dedup.
		{"validating_same_service_twice", validating(svcRef(new(int32(443))), svcRef(new(int32(443)))), []serviceRef{ref(443), ref(443)}},
		{"validating_different_ports", validating(svcRef(new(int32(443))), svcRef(new(int32(8443)))), []serviceRef{ref(443), ref(8443)}},
		{"mutating", &admissionregistrationv1.MutatingWebhookConfiguration{Webhooks: []admissionregistrationv1.MutatingWebhook{
			{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: svcRef(new(int32(8443)))}},
			{ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: new("https://x")}},
		}}, []serviceRef{ref(8443)}},
		{"apiservice", &apiregistrationv1.APIService{Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{Namespace: "ns", Name: "svc", Port: new(int32(443))},
		}}, []serviceRef{ref(443)}},
		{"apiservice_local", &apiregistrationv1.APIService{}, nil},
		{"crd_conversion_webhook", crd(apiextensionsv1.WebhookConverter, &apiextensionsv1.WebhookClientConfig{
			Service: &apiextensionsv1.ServiceReference{Namespace: "ns", Name: "svc", Port: new(int32(443))},
		}), []serviceRef{ref(443)}},
		{"crd_no_conversion", &apiextensionsv1.CustomResourceDefinition{}, nil},
		{"crd_strategy_none", crd(apiextensionsv1.NoneConverter, nil), nil},
		{"crd_url_only", crd(apiextensionsv1.WebhookConverter, &apiextensionsv1.WebhookClientConfig{URL: new("https://x")}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := serviceRefs(c.in); !slices.Equal(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDiscoveryToServices_Dedups(t *testing.T) {
	port := new(int32(443))
	cfg := &admissionregistrationv1.ValidatingWebhookConfiguration{Webhooks: []admissionregistrationv1.ValidatingWebhook{
		{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "a", Port: port}}},
		{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "b", Port: port}}},
		{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "a", Port: new(int32(8443))}}},
	}}
	got := discoveryToServices(context.Background(), cfg)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("got %v, want one request each for ns/a and ns/b", got)
	}
}

// ---- policy builder ----

func TestBuildApiserverReachablePolicy(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager-webhook", Namespace: "cert-manager"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "webhook"},
			Ports: []corev1.ServicePort{
				{Port: 443, TargetPort: intstr.FromInt32(10250)},
			},
		},
	}

	p, err := buildApiserverReachablePolicy(svc, nil, []int32{443}, "0.0.0.0/0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.Namespace != "cert-manager" {
		t.Errorf("namespace = %q, want cert-manager", p.Namespace)
	}
	if p.Labels[LabelSourceKind] != LabelSourceKindApiserver {
		t.Errorf("source-kind label = %q, want %q", p.Labels[LabelSourceKind], LabelSourceKindApiserver)
	}
	if p.Labels[LabelSource] != "apiserver-cert-manager-webhook" {
		t.Errorf("source label = %q, want apiserver-cert-manager-webhook", p.Labels[LabelSource])
	}
	if p.Labels[LabelRole] != LabelRoleExternalAllow {
		t.Errorf("role label = %q, want external-allow", p.Labels[LabelRole])
	}
	if !maps.Equal(p.Spec.PodSelector.MatchLabels, map[string]string{"app": "webhook"}) {
		t.Errorf("podSelector matchLabels mismatch: %v", p.Spec.PodSelector.MatchLabels)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 1 || p.Spec.Ingress[0].From[0].IPBlock == nil {
		t.Fatalf("unexpected ingress shape: %+v", p.Spec.Ingress)
	}
}

func TestBuildApiserverReachablePolicy_PortsAndCIDR(t *testing.T) {
	svcWith := func(ports ...corev1.ServicePort) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "x"}, Ports: ports},
		}
	}
	numeric := func(port, target int32) corev1.ServicePort {
		return corev1.ServicePort{Port: port, TargetPort: intstr.FromInt32(target)}
	}
	named := corev1.ServicePort{Port: 443, TargetPort: intstr.FromString("https")}
	// Selected by {app: x}.
	pod := func(name string, port int32) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Labels: map[string]string{"app": "x"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: []corev1.ContainerPort{{Name: name, ContainerPort: port}}}}},
		}
	}
	unselected := pod("https", 9999)
	unselected.Labels = map[string]string{"app": "y"}

	cases := []struct {
		name           string
		svc            *corev1.Service
		pods           []corev1.Pod
		ports          []int32
		cidr, wantCIDR string
		want           []int
		wantUnresolved bool
	}{
		{name: "target_port", svc: svcWith(numeric(443, 10250)), ports: []int32{443}, want: []int{10250}},
		{name: "custom_cidr", svc: svcWith(numeric(443, 443)), ports: []int32{443}, cidr: "10.0.0.0/8", wantCIDR: "10.0.0.0/8", want: []int{443}},
		{name: "empty_cidr_is_all", svc: svcWith(numeric(443, 443)), ports: []int32{443}, cidr: "", want: []int{443}},
		// Ports are emitted in the order passed.
		{name: "multiple_ports", svc: svcWith(numeric(443, 8443), numeric(8080, 8080)), ports: []int32{443, 8080}, want: []int{8443, 8080}},
		// cert-manager: `targetPort: webhook-tls` backed by 10250. Allowing
		// 443 instead would time out admission requests DNAT'd to the pod.
		{name: "named_target_port_from_pod", svc: svcWith(named), pods: []corev1.Pod{pod("https", 10250)}, ports: []int32{443}, want: []int{10250}},
		// Only a pod matching both the selector and the port name counts.
		{name: "named_target_port_across_pods", svc: svcWith(named),
			pods: []corev1.Pod{unselected, pod("metrics", 9090), pod("https", 8443)}, ports: []int32{443}, want: []int{8443}},
		// The sentinel makes the caller requeue instead of emitting the wrong port.
		{name: "named_target_port_no_pod", svc: svcWith(named), ports: []int32{443}, wantUnresolved: true},
		{name: "named_target_port_wrong_name", svc: svcWith(named), pods: []corev1.Pod{pod("other", 10250)}, ports: []int32{443}, wantUnresolved: true},
		// A discovery port the Service doesn't declare passes through.
		{name: "port_not_in_spec", svc: svcWith(numeric(443, 443)), ports: []int32{8443}, want: []int{8443}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := buildApiserverReachablePolicy(c.svc, c.pods, c.ports, c.cidr)
			if c.wantUnresolved {
				if !errors.Is(err, errNamedPortUnresolvable) {
					t.Fatalf("err = %v, want errNamedPortUnresolvable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			wantCIDR := cmp.Or(c.wantCIDR, "0.0.0.0/0")
			if got := p.Spec.Ingress[0].From[0].IPBlock.CIDR; got != wantCIDR {
				t.Errorf("CIDR = %q, want %q", got, wantCIDR)
			}
			var got []int
			for _, pp := range p.Spec.Ingress[0].Ports {
				got = append(got, pp.Port.IntValue())
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("ports = %v, want %v", got, c.want)
			}
		})
	}
}

// ---- annotation opt-in / opt-out ----

func TestApiserverReachableOptedIn(t *testing.T) {
	cases := []struct {
		annotations map[string]string
		want        bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{AnnotationApiserverReachable: "true"}, true},
		{map[string]string{AnnotationApiserverReachable: "false"}, false},
		{map[string]string{AnnotationApiserverReachable: "TRUE"}, false}, // strict literal
		{map[string]string{AnnotationApiserverReachable: ""}, false},
	}
	for _, c := range cases {
		if got := ApiserverReachableOptedIn(c.annotations); got != c.want {
			t.Errorf("ApiserverReachableOptedIn(%v) = %v, want %v", c.annotations, got, c.want)
		}
	}
}

// ---- Reconcile ----

// optedInService returns a Service annotated apiserver-reachable.
func optedInService(ns, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Annotations: map[string]string{AnnotationApiserverReachable: "true"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports:    []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(8443)}},
		},
	}
}

// The apiserver dials an ExternalName Service's DNS target, not its pods, so
// a stray selector must not produce a policy for them.
func TestApiserverReachableReconcile_ExternalName_NoPolicy(t *testing.T) {
	s := optedInService("ns", "webhook")
	s.Spec.Type = corev1.ServiceTypeExternalName
	s.Spec.ExternalName = "webhook.example.com"
	c := autoAllowClient(t, mkNamespace("ns", nil), s)
	r := &ApiserverReachableReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil)}
	if err := reconcileName(t, r.Reconcile, "ns", "webhook"); err != nil {
		t.Fatal(err)
	}
	if got := listPolicies(t, c, "ns"); len(got) != 0 {
		t.Errorf("got %d policies for an ExternalName Service, want 0", len(got))
	}
}
