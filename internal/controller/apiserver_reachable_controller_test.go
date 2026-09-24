package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
				cc.URL = ptr("https://external.example.com/validate")
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
		{"validating", validating(svcRef(ptr[int32](8443))), []serviceRef{ref(8443)}},
		{"validating_port_defaulted_to_443", validating(svcRef(nil)), []serviceRef{ref(443)}},
		{"validating_url_only_skipped", validating(nil), nil},
		// One ref per webhook entry; the callers dedup.
		{"validating_same_service_twice", validating(svcRef(ptr[int32](443)), svcRef(ptr[int32](443))), []serviceRef{ref(443), ref(443)}},
		{"validating_different_ports", validating(svcRef(ptr[int32](443)), svcRef(ptr[int32](8443))), []serviceRef{ref(443), ref(8443)}},
		{"mutating", &admissionregistrationv1.MutatingWebhookConfiguration{Webhooks: []admissionregistrationv1.MutatingWebhook{
			{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: svcRef(ptr[int32](8443))}},
			{ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: ptr("https://x")}},
		}}, []serviceRef{ref(8443)}},
		{"apiservice", &apiregistrationv1.APIService{Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{Namespace: "ns", Name: "svc", Port: ptr[int32](443)},
		}}, []serviceRef{ref(443)}},
		{"apiservice_local", &apiregistrationv1.APIService{}, nil},
		{"crd_conversion_webhook", crd(apiextensionsv1.WebhookConverter, &apiextensionsv1.WebhookClientConfig{
			Service: &apiextensionsv1.ServiceReference{Namespace: "ns", Name: "svc", Port: ptr[int32](443)},
		}), []serviceRef{ref(443)}},
		{"crd_no_conversion", &apiextensionsv1.CustomResourceDefinition{}, nil},
		{"crd_strategy_none", crd(apiextensionsv1.NoneConverter, nil), nil},
		{"crd_url_only", crd(apiextensionsv1.WebhookConverter, &apiextensionsv1.WebhookClientConfig{URL: ptr("https://x")}), nil},
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
	port := ptr[int32](443)
	cfg := &admissionregistrationv1.ValidatingWebhookConfiguration{Webhooks: []admissionregistrationv1.ValidatingWebhook{
		{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "a", Port: port}}},
		{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "b", Port: port}}},
		{ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "ns", Name: "a", Port: ptr[int32](8443)}}},
	}}
	got := discoveryToServices(context.Background(), cfg)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("got %v, want one request each for ns/a and ns/b", got)
	}
}

// ---- policy builder ----

// podWith returns a Pod matching the standard test selector
// {app: x} and with containers exposing the given (name, port) pairs.
func podWith(ns string, namedPorts ...corev1.ContainerPort) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p", Labels: map[string]string{"app": "x"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "c",
			Ports: namedPorts,
		}}},
	}
}

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
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 1 {
		t.Fatalf("unexpected ingress shape: %+v", p.Spec.Ingress)
	}
	if p.Spec.Ingress[0].From[0].IPBlock == nil ||
		p.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("ipBlock CIDR mismatch: %+v", p.Spec.Ingress[0].From[0].IPBlock)
	}
	if len(p.Spec.Ingress[0].Ports) != 1 ||
		p.Spec.Ingress[0].Ports[0].Port.IntValue() != 10250 {
		t.Errorf("port mismatch: %+v", p.Spec.Ingress[0].Ports)
	}
}

func TestBuildApiserverReachablePolicy_CustomCIDR(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports:    []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(443)}},
		},
	}
	p, err := buildApiserverReachablePolicy(svc, nil, []int32{443}, "10.0.0.0/8")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.Spec.Ingress[0].From[0].IPBlock.CIDR != "10.0.0.0/8" {
		t.Errorf("custom CIDR not honored: got %q", p.Spec.Ingress[0].From[0].IPBlock.CIDR)
	}
}

func TestBuildApiserverReachablePolicy_EmptyCIDRDefaultsToAll(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports:    []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(443)}},
		},
	}
	p, err := buildApiserverReachablePolicy(svc, nil, []int32{443}, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("empty CIDR should default to 0.0.0.0/0, got %q", p.Spec.Ingress[0].From[0].IPBlock.CIDR)
	}
}

func TestBuildApiserverReachablePolicy_MultiplePorts(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports: []corev1.ServicePort{
				{Port: 443, TargetPort: intstr.FromInt32(8443)},
				{Port: 8080, TargetPort: intstr.FromInt32(8080)},
			},
		},
	}
	p, err := buildApiserverReachablePolicy(svc, nil, []int32{443, 8080}, "0.0.0.0/0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(p.Spec.Ingress[0].Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(p.Spec.Ingress[0].Ports))
	}
	// Ports are emitted in the order passed; both targetPorts resolved.
	if p.Spec.Ingress[0].Ports[0].Port.IntValue() != 8443 {
		t.Errorf("first port targetPort mismatch: %v", p.Spec.Ingress[0].Ports[0].Port)
	}
	if p.Spec.Ingress[0].Ports[1].Port.IntValue() != 8080 {
		t.Errorf("second port targetPort mismatch: %v", p.Spec.Ingress[0].Ports[1].Port)
	}
}

// The cert-manager case: `targetPort: webhook-tls` backed by containerPort
// 10250. The policy must allow 10250, not the Service-side 443, or admission
// requests DNAT'd to the pod time out.
func TestBuildApiserverReachablePolicy_NamedTargetPortResolvedFromPod(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports: []corev1.ServicePort{
				{Port: 443, TargetPort: intstr.FromString("webhook-tls")},
			},
		},
	}
	pod := podWith("ns", corev1.ContainerPort{Name: "webhook-tls", ContainerPort: 10250})

	p, err := buildApiserverReachablePolicy(svc, []corev1.Pod{pod}, []int32{443}, "0.0.0.0/0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := p.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 10250 {
		t.Errorf("named targetPort 'webhook-tls' should resolve to pod containerPort 10250, got %d", got)
	}
}

// No backing pod yet, or no matching containerPort name: the sentinel error
// makes the caller requeue instead of emitting the wrong port.
func TestBuildApiserverReachablePolicy_NamedTargetPortUnresolvableReturnsError(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports: []corev1.ServicePort{
				{Port: 443, TargetPort: intstr.FromString("webhook-tls")},
			},
		},
	}

	// Case A: no pods at all.
	_, err := buildApiserverReachablePolicy(svc, nil, []int32{443}, "0.0.0.0/0")
	if !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("no pods → err = %v, want errNamedPortUnresolvable", err)
	}

	// Case B: matching pod exists but its containerPort name doesn't
	// match what the Service declared. Still unresolvable.
	wrongName := podWith("ns", corev1.ContainerPort{Name: "other", ContainerPort: 10250})
	_, err = buildApiserverReachablePolicy(svc, []corev1.Pod{wrongName}, []int32{443}, "0.0.0.0/0")
	if !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("wrong containerPort name → err = %v, want errNamedPortUnresolvable", err)
	}
}

// Only one of several pods matches both the selector and the port name.
func TestBuildApiserverReachablePolicy_NamedTargetPortResolvedAcrossMultiplePods(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports: []corev1.ServicePort{
				{Port: 443, TargetPort: intstr.FromString("https")},
			},
		},
	}
	// Pod with wrong app label — should be skipped (selector mismatch).
	noMatchSelector := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "other", Labels: map[string]string{"app": "y"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "c",
			Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: 9999}},
		}}},
	}
	// Pod with matching selector but wrong containerPort name.
	noMatchPortName := podWith("ns", corev1.ContainerPort{Name: "metrics", ContainerPort: 9090})
	// The right one.
	right := podWith("ns", corev1.ContainerPort{Name: "https", ContainerPort: 8443})
	right.Name = "right"

	p, err := buildApiserverReachablePolicy(svc, []corev1.Pod{noMatchSelector, noMatchPortName, right}, []int32{443}, "0.0.0.0/0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := p.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8443 {
		t.Errorf("expected port 8443 from matching pod, got %d", got)
	}
}

func TestBuildApiserverReachablePolicy_ServicePortNotInSpec(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "x"},
			Ports:    []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(443)}},
		},
	}
	// Discovery says port 8443 but Service only declares 443. Pass the
	// discovery port through (kube-proxy DNAT will fail anyway since
	// 8443 maps nowhere, but emitting the policy leaves a breadcrumb).
	p, err := buildApiserverReachablePolicy(svc, nil, []int32{8443}, "0.0.0.0/0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.Spec.Ingress[0].Ports[0].Port.IntValue() != 8443 {
		t.Errorf("unknown discovery port should pass through, got %v", p.Spec.Ingress[0].Ports[0].Port)
	}
}

// ---- name + naming ----

func TestApiserverReachablePolicyName_ShapeAndUniqueness(t *testing.T) {
	a := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "webhook", Namespace: "ns-a"}}
	b := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "webhook", Namespace: "ns-b"}}

	na := apiserverReachablePolicyName(a)
	nb := apiserverReachablePolicyName(b)
	if na == nb {
		t.Errorf("same-name Services in different NSes should produce different policy names (got %q == %q)", na, nb)
	}
	if len(na) > 63 {
		t.Errorf("policy name %q exceeds K8s 63-char limit", na)
	}
	if !strings.HasPrefix(na, "kube-vnet.ext.apiserver.webhook-") {
		t.Errorf("policy name shape unexpected: %q", na)
	}
}

func TestApiserverReachablePolicyName_LongServiceTruncates(t *testing.T) {
	longName := "this-is-a-very-very-long-service-name-that-exceeds-the-K8s-name-limit"
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: longName, Namespace: "ns"}}
	n := apiserverReachablePolicyName(svc)
	if len(n) > 63 {
		t.Errorf("policy name %q exceeds K8s 63-char limit", n)
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

// reconcileApiserverReachable runs one reconcile of Service ns/name against
// objs and returns the client.
func reconcileApiserverReachable(t *testing.T, ns, name string, objs ...client.Object) client.Client {
	t.Helper()
	c := autoAllowClient(t, objs...)
	r := &ApiserverReachableReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: &fakeRecorder{}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return c
}

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

func TestApiserverReachableReconcile_AppliesPolicy(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
	c := reconcileApiserverReachable(t, "ns", "webhook", ns, optedInService("ns", "webhook"))
	if got := listPolicies(t, c, "ns"); len(got) != 1 {
		t.Errorf("got %d policies, want 1", len(got))
	}
}

// The apiserver dials an ExternalName Service's DNS target, not its pods, so
// a stray selector must not produce a policy for them.
func TestApiserverReachableReconcile_ExternalName_NoPolicy(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
	s := optedInService("ns", "webhook")
	s.Spec.Type = corev1.ServiceTypeExternalName
	s.Spec.ExternalName = "webhook.example.com"
	c := reconcileApiserverReachable(t, "ns", "webhook", ns, s)
	if got := listPolicies(t, c, "ns"); len(got) != 0 {
		t.Errorf("got %d policies for an ExternalName Service, want 0", len(got))
	}
}

// NamespaceLifecycle admission rejects creates in a terminating namespace, so
// applying there would only fail and retry until the namespace is gone.
func TestApiserverReachableReconcile_TerminatingNamespace_NoApply(t *testing.T) {
	c := reconcileApiserverReachable(t, "ns", "webhook", terminatingNamespace("ns"), optedInService("ns", "webhook"))
	if got := listPolicies(t, c, "ns"); len(got) != 0 {
		t.Errorf("applied %d policies into a terminating namespace", len(got))
	}
}

func ptr[T any](v T) *T { return &v }
