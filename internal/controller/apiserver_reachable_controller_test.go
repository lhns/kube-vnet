package controller

import (
	"errors"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

// ---- extractors ----

func TestExtractValidatingWebhookRefs(t *testing.T) {
	port443 := int32(443)
	port8443 := int32(8443)

	cases := []struct {
		name string
		in   *admissionregistrationv1.ValidatingWebhookConfiguration
		want []serviceRef
	}{
		{
			name: "service_ref_present",
			in: &admissionregistrationv1.ValidatingWebhookConfiguration{
				Webhooks: []admissionregistrationv1.ValidatingWebhook{{
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Namespace: "cert-manager", Name: "cert-manager-webhook",
							Port: &port443,
						},
					},
				}},
			},
			want: []serviceRef{{Namespace: "cert-manager", Name: "cert-manager-webhook", Port: 443}},
		},
		{
			name: "port_defaulted_to_443",
			in: &admissionregistrationv1.ValidatingWebhookConfiguration{
				Webhooks: []admissionregistrationv1.ValidatingWebhook{{
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Namespace: "ns", Name: "svc",
							Port: nil,
						},
					},
				}},
			},
			want: []serviceRef{{Namespace: "ns", Name: "svc", Port: 443}},
		},
		{
			name: "url_only_skipped",
			in: &admissionregistrationv1.ValidatingWebhookConfiguration{
				Webhooks: []admissionregistrationv1.ValidatingWebhook{{
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						URL: ptr("https://external.example.com/validate"),
					},
				}},
			},
			want: []serviceRef{},
		},
		{
			name: "multiple_webhook_entries_same_service",
			in: &admissionregistrationv1.ValidatingWebhookConfiguration{
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Namespace: "ns", Name: "svc", Port: &port443,
						},
					}},
					{ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Namespace: "ns", Name: "svc", Port: &port443,
						},
					}},
				},
			},
			// Extractor returns 1 ref per webhook entry; dedup happens at
			// the reconciler. Both refs present.
			want: []serviceRef{
				{Namespace: "ns", Name: "svc", Port: 443},
				{Namespace: "ns", Name: "svc", Port: 443},
			},
		},
		{
			name: "service_ref_with_different_ports",
			in: &admissionregistrationv1.ValidatingWebhookConfiguration{
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Namespace: "ns", Name: "svc", Port: &port443,
						},
					}},
					{ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Namespace: "ns", Name: "svc", Port: &port8443,
						},
					}},
				},
			},
			want: []serviceRef{
				{Namespace: "ns", Name: "svc", Port: 443},
				{Namespace: "ns", Name: "svc", Port: 8443},
			},
		},
		{name: "nil_input", in: nil, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractValidatingWebhookRefs(c.in)
			if !sliceEqualServiceRef(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestExtractMutatingWebhookRefs(t *testing.T) {
	port := int32(8443)
	in := &admissionregistrationv1.MutatingWebhookConfiguration{
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Namespace: "istio-system", Name: "istiod",
					Port: &port,
				},
			},
		}},
	}
	got := extractMutatingWebhookRefs(in)
	want := []serviceRef{{Namespace: "istio-system", Name: "istiod", Port: 8443}}
	if !sliceEqualServiceRef(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// URL-only is skipped
	urlOnly := &admissionregistrationv1.MutatingWebhookConfiguration{
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: ptr("https://x")},
		}},
	}
	if len(extractMutatingWebhookRefs(urlOnly)) != 0 {
		t.Errorf("URL-only webhook should be skipped")
	}
}

func TestExtractAPIServiceRefs(t *testing.T) {
	port := int32(443)
	in := &apiregistrationv1.APIService{
		Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{
				Namespace: "kube-system", Name: "metrics-server",
				Port: &port,
			},
		},
	}
	got := extractAPIServiceRefs(in)
	want := []serviceRef{{Namespace: "kube-system", Name: "metrics-server", Port: 443}}
	if !sliceEqualServiceRef(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// Local APIService (no service ref) → no emission.
	local := &apiregistrationv1.APIService{Spec: apiregistrationv1.APIServiceSpec{Service: nil}}
	if len(extractAPIServiceRefs(local)) != 0 {
		t.Errorf("local APIService should yield no refs")
	}
}

func TestExtractCRDConversionRefs(t *testing.T) {
	port := int32(443)

	// Conversion webhook with Service ref → extracted.
	withSvc := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Namespace: "kubevirt", Name: "kubevirt-webhook",
							Port: &port,
						},
					},
				},
			},
		},
	}
	got := extractCRDConversionRefs(withSvc)
	want := []serviceRef{{Namespace: "kubevirt", Name: "kubevirt-webhook", Port: 443}}
	if !sliceEqualServiceRef(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// No conversion configured → no refs.
	noConv := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Conversion: nil},
	}
	if len(extractCRDConversionRefs(noConv)) != 0 {
		t.Errorf("CRD without conversion should yield no refs")
	}

	// Strategy: None → no refs even if webhook block present.
	noneStrategy := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.NoneConverter,
			},
		},
	}
	if len(extractCRDConversionRefs(noneStrategy)) != 0 {
		t.Errorf("CRD with strategy=None should yield no refs")
	}

	// URL-only conversion webhook → no refs.
	urlOnly := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						URL: ptr("https://x"),
					},
				},
			},
		},
	}
	if len(extractCRDConversionRefs(urlOnly)) != 0 {
		t.Errorf("URL-only conversion webhook should yield no refs")
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
	if !mapsEqual(p.Spec.PodSelector.MatchLabels, map[string]string{"app": "webhook"}) {
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

// TestBuildApiserverReachablePolicy_NamedTargetPortResolvedFromPod —
// the cert-manager case: Service uses `targetPort: webhook-tls`, the
// backing Pod exposes containerPort 10250 named webhook-tls, and the
// emitted policy MUST use 10250 (not the Service-side 443).
//
// Pre-fix, this returned 443 — admission requests were DNAT'd to pod-port
// 10250 but the policy only allowed 443, so the apiserver-to-webhook
// connection silently timed out. Regression test for the user-reported
// bug after ADR 0041 shipped.
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

// TestBuildApiserverReachablePolicy_NamedTargetPortUnresolvableReturnsError —
// no backing pod yet, or no matching containerPort name. Returns the
// sentinel error so the caller emits a Pending event and requeues.
// Without this contract the policy would silently emit with the wrong
// port (the original ADR 0041 bug).
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

// TestBuildApiserverReachablePolicy_NamedTargetPortResolvedAcrossMultiplePods
// covers the edge case where the namespace has many pods but only one
// matches both the Service selector AND has the matching containerPort
// name. resolveTargetPort iterates and finds it.
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
	if !startsWith(na, "kube-vnet.ext.apiserver.webhook-") {
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

// ---- helpers ----

func ptr[T any](v T) *T { return &v }

func sliceEqualServiceRef(a, b []serviceRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
