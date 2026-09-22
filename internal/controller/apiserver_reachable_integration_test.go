//go:build integration

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// makeWebhookService creates a ClusterIP Service backing a "webhook" pod.
// Used by the ADR 0041 tests as the apiserver-dialed Service.
func makeWebhookService(ns, name string, ports ...corev1.ServicePort) *corev1.Service {
	if len(ports) == 0 {
		ports = []corev1.ServicePort{
			{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
		}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": name},
			Ports:    ports,
		},
	}
}

func apiserverReachablePolicyNameFor(ns, name string) string {
	return apiserverReachablePolicyName(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	})
}

func waitForApiserverReachablePolicy(t *testing.T, ns, svcName string, timeout time.Duration) *networkingv1.NetworkPolicy {
	t.Helper()
	return waitForPolicy(t, ns, apiserverReachablePolicyNameFor(ns, svcName), timeout)
}

func waitForApiserverReachablePolicyAbsent(t *testing.T, ns, svcName string, timeout time.Duration) {
	t.Helper()
	waitForPolicyAbsent(t, ns, apiserverReachablePolicyNameFor(ns, svcName), timeout)
}

// serviceWebhookClientConfig points a webhook at port 443 of Service ns/svc.
func serviceWebhookClientConfig(ns, svc string) admissionregistrationv1.WebhookClientConfig {
	port := int32(443)
	return admissionregistrationv1.WebhookClientConfig{
		Service: &admissionregistrationv1.ServiceReference{Namespace: ns, Name: svc, Port: &port},
	}
}

// mustCreateValidatingWebhook creates a single-webhook
// ValidatingWebhookConfiguration calling cc and deletes it on cleanup.
func mustCreateValidatingWebhook(t *testing.T, name string, cc admissionregistrationv1.WebhookClientConfig) *admissionregistrationv1.ValidatingWebhookConfiguration {
	t.Helper()
	side := admissionregistrationv1.SideEffectClassNone
	whc := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name:                    "test.example.com",
			ClientConfig:            cc,
			SideEffects:             &side,
			AdmissionReviewVersions: []string{"v1"},
		}},
	}
	mustCreate(t, whc)
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), whc) })
	return whc
}

func TestIntegration_ApiserverReachable_ValidatingWHC_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "ar-vwhc")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeWebhookService(ns, "webhook")
	mustCreate(t, svc)

	mustCreateValidatingWebhook(t, "ar-vwhc-"+ns, serviceWebhookClientConfig(ns, "webhook"))

	pol := waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)
	if pol.Labels[LabelSourceKind] != LabelSourceKindApiserver {
		t.Errorf("source-kind label = %q, want %q", pol.Labels[LabelSourceKind], LabelSourceKindApiserver)
	}
	if pol.Spec.Ingress[0].From[0].IPBlock == nil ||
		pol.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("ipBlock CIDR mismatch: %+v", pol.Spec.Ingress[0].From[0].IPBlock)
	}
	// Service has targetPort 8443 for port 443; the emitted policy should
	// scope to the pod-side targetPort.
	if pol.Spec.Ingress[0].Ports[0].Port.IntValue() != 8443 {
		t.Errorf("expected pod-side targetPort 8443, got %v", pol.Spec.Ingress[0].Ports[0].Port)
	}
}

func TestIntegration_ApiserverReachable_MutatingWHC_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "ar-mwhc")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "injector"))

	port443 := int32(443)
	side := admissionregistrationv1.SideEffectClassNone
	mustCreate(t, &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "ar-mwhc-" + ns},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name: "inject.example.com",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Namespace: ns, Name: "injector", Port: &port443,
				},
			},
			SideEffects:             &side,
			AdmissionReviewVersions: []string{"v1"},
		}},
	})
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(),
			&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "ar-mwhc-" + ns}})
	})

	waitForApiserverReachablePolicy(t, ns, "injector", 10*time.Second)
}

func TestIntegration_ApiserverReachable_APIService_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "ar-apisvc")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "metrics"))

	port443 := int32(443)
	mustCreate(t, &apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: "v1beta1.example.com"},
		Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{
				Namespace: ns, Name: "metrics", Port: &port443,
			},
			Group:                 "example.com",
			Version:               "v1beta1",
			GroupPriorityMinimum:  1000,
			VersionPriority:       15,
			InsecureSkipTLSVerify: true,
		},
	})
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(),
			&apiregistrationv1.APIService{ObjectMeta: metav1.ObjectMeta{Name: "v1beta1.example.com"}})
	})

	waitForApiserverReachablePolicy(t, ns, "metrics", 10*time.Second)
}

func TestIntegration_ApiserverReachable_CRDConversion_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "ar-crdconv")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "converter"))

	port443 := int32(443)
	path := "/convert"
	mustCreate(t, &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
				},
			}},
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Namespace: ns, Name: "converter", Port: &port443, Path: &path,
						},
					},
				},
			},
		},
	})
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(),
			&apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"}})
	})

	waitForApiserverReachablePolicy(t, ns, "converter", 10*time.Second)
}

func TestIntegration_ApiserverReachable_AnnotationOptIn_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "ar-anno")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeWebhookService(ns, "annotated")
	svc.Annotations = map[string]string{AnnotationApiserverReachable: "true"}
	mustCreate(t, svc)

	// No discovery resource — only the annotation. Should still get a policy.
	pol := waitForApiserverReachablePolicy(t, ns, "annotated", 10*time.Second)
	if pol.Labels[LabelSourceKind] != LabelSourceKindApiserver {
		t.Errorf("source-kind label = %q, want %q", pol.Labels[LabelSourceKind], LabelSourceKindApiserver)
	}
}

func TestIntegration_ApiserverReachable_NoEmissionWithoutDiscoveryOrAnnotation(t *testing.T) {
	ns := uniqueNS(t, "ar-bare")
	mustCreate(t, makeNamespace(ns, nil, nil))

	// Bare Service, no annotation, no discovery resource.
	mustCreate(t, makeWebhookService(ns, "bare"))

	assertPolicyStaysAbsent(t, ns, apiserverReachablePolicyNameFor(ns, "bare"), 2*time.Second)
}

func TestIntegration_ApiserverReachable_DiscoveryDeleted_PolicySwept(t *testing.T) {
	ns := uniqueNS(t, "ar-delete")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "webhook"))

	whc := mustCreateValidatingWebhook(t, "ar-delete-"+ns, serviceWebhookClientConfig(ns, "webhook"))
	waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)

	if err := testClient.Delete(context.Background(), whc); err != nil {
		t.Fatalf("delete WHC: %v", err)
	}
	waitForApiserverReachablePolicyAbsent(t, ns, "webhook", 10*time.Second)
}

func TestIntegration_ApiserverReachable_OptOut_ServiceAnnotation(t *testing.T) {
	ns := uniqueNS(t, "ar-optout-svc")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "webhook"))

	mustCreateValidatingWebhook(t, "ar-optout-svc-"+ns, serviceWebhookClientConfig(ns, "webhook"))
	waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)

	updateService(t, ns, "webhook", func(s *corev1.Service) {
		metav1.SetMetaDataAnnotation(&s.ObjectMeta, AnnotationExternalAllow, "false")
	})
	waitForApiserverReachablePolicyAbsent(t, ns, "webhook", 10*time.Second)
}

func TestIntegration_ApiserverReachable_DriftCorrection(t *testing.T) {
	ns := uniqueNS(t, "ar-drift")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "webhook"))

	mustCreateValidatingWebhook(t, "ar-drift-"+ns, serviceWebhookClientConfig(ns, "webhook"))
	pol := waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)

	// Delete the policy by hand. Operator should recreate it.
	if err := testClient.Delete(context.Background(), pol); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)
}

func TestIntegration_ApiserverReachable_URLOnlyWebhook_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "ar-urlonly")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeWebhookService(ns, "would-not-be-target"))

	url := "https://external.example.com/validate"
	mustCreateValidatingWebhook(t, "ar-urlonly-"+ns, admissionregistrationv1.WebhookClientConfig{URL: &url})

	assertPolicyStaysAbsent(t, ns, apiserverReachablePolicyNameFor(ns, "would-not-be-target"), 2*time.Second)
}

// makeWebhookPod returns a pause-image Pod matching the makeWebhookService
// selector with the given (name, containerPort) pairs. Used by the
// named-targetPort integration tests so kube-vnet's resolver can find
// the actual pod-side port.
func makeWebhookPod(ns, svcName string, ports ...corev1.ContainerPort) *corev1.Pod {
	pod := makePod(ns, "webhook-backend", map[string]string{"app": svcName})
	pod.Spec.Containers[0].Ports = ports
	return pod
}

// TestIntegration_ApiserverReachable_NamedTargetPort_ResolvedFromPod
// regression test for the user-reported bug after ADR 0041 shipped:
// cert-manager-webhook's Service uses `targetPort: webhook-tls` (named
// string port). kube-proxy DNATs to the pod-side containerPort (10250),
// so the emitted NetworkPolicy must allow port 10250, NOT the Service-
// side 443. Before the fix the policy allowed 443 and admission silently
// timed out with `context deadline exceeded`.
func TestIntegration_ApiserverReachable_NamedTargetPort_ResolvedFromPod(t *testing.T) {
	ns := uniqueNS(t, "ar-named")
	mustCreate(t, makeNamespace(ns, nil, nil))

	// Service with NAMED targetPort.
	svc := makeWebhookService(ns, "webhook",
		corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromString("webhook-tls"), Protocol: corev1.ProtocolTCP},
	)
	mustCreate(t, svc)

	// Backing pod exposing the named containerPort.
	mustCreate(t, makeWebhookPod(ns, "webhook",
		corev1.ContainerPort{Name: "webhook-tls", ContainerPort: 10250},
	))

	mustCreateValidatingWebhook(t, "ar-named-"+ns, serviceWebhookClientConfig(ns, "webhook"))

	pol := waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 10250 {
		t.Errorf("named targetPort `webhook-tls` should resolve to containerPort 10250, got %d; policy=%+v",
			got, pol.Spec.Ingress[0].Ports)
	}
}

// TestIntegration_ApiserverReachable_NamedTargetPort_Pending_Then_PodAppears
// covers the Service-before-Pod ordering. Without the Pod-create watcher
// the policy would only appear after the 30s requeue; with the watcher
// it appears as soon as the matching pod is created.
func TestIntegration_ApiserverReachable_NamedTargetPort_Pending_Then_PodAppears(t *testing.T) {
	ns := uniqueNS(t, "ar-pending")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeWebhookService(ns, "webhook",
		corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromString("webhook-tls"), Protocol: corev1.ProtocolTCP},
	)
	mustCreate(t, svc)

	mustCreateValidatingWebhook(t, "ar-pending-"+ns, serviceWebhookClientConfig(ns, "webhook"))

	// No pod yet: the named port is unresolvable, and a policy on the wrong
	// port would be worse than none.
	assertPolicyStaysAbsent(t, ns, apiserverReachablePolicyNameFor(ns, "webhook"), 2*time.Second)

	// The Pod watch re-enqueues the Service.
	mustCreate(t, makeWebhookPod(ns, "webhook",
		corev1.ContainerPort{Name: "webhook-tls", ContainerPort: 10250},
	))

	pol := waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 10250 {
		t.Errorf("after Pod create, named targetPort should resolve to 10250, got %d", got)
	}
}

// TestIntegration_ApiserverReachable_SurvivesExternalAllowReconcile is the
// regression test for the cross-reconciler deletion bug found in the
// project audit: the ExternalAllowReconciler's owner-ref sweeps filtered
// only on role=external-allow (no source-kind), so reconciling a
// not-externally-exposed webhook Service (plain ClusterIP — the exact
// cert-manager-webhook shape) deleted the ApiserverReachableReconciler's
// policy for the same Service on every pass. The drift watch recreated
// it, producing a permanent delete/recreate loop with windows where the
// apiserver→webhook allow was absent.
//
// The fix exempts other-source-kind policies via claimedByOtherSourceKind.
// This test asserts the policy's UID stays STABLE across ExternalAllow
// reconciles — existence alone would pass even under thrash, because the
// drift watch recreates within milliseconds.
func TestIntegration_ApiserverReachable_SurvivesExternalAllowReconcile(t *testing.T) {
	ns := uniqueNS(t, "ar-coexist")
	mustCreate(t, makeNamespace(ns, nil, nil))

	// Plain ClusterIP webhook Service — NOT externally exposed, so every
	// ExternalAllowReconciler pass takes the deletePolicyForService path.
	mustCreate(t, makeWebhookService(ns, "webhook"))

	mustCreateValidatingWebhook(t, "ar-coexist-"+ns, serviceWebhookClientConfig(ns, "webhook"))

	pol := waitForApiserverReachablePolicy(t, ns, "webhook", 10*time.Second)
	originalUID := pol.UID

	// Force several ExternalAllowReconciler passes via inert annotation
	// flips on the Service (Service updates are its primary trigger).
	for i := 0; i < 3; i++ {
		updateService(t, ns, "webhook", func(s *corev1.Service) {
			metav1.SetMetaDataAnnotation(&s.ObjectMeta, "test-trigger", fmt.Sprintf("pass-%d", i))
		})
		time.Sleep(1 * time.Second)
	}

	// The policy must still exist AND be the SAME object (UID unchanged) —
	// a delete/recreate cycle would produce a new UID.
	var after networkingv1.NetworkPolicy
	if err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: apiserverReachablePolicyNameFor(ns, "webhook")}, &after); err != nil {
		t.Fatalf("apiserver-reachable policy missing after ExternalAllow passes: %v", err)
	}
	if after.UID != originalUID {
		t.Errorf("policy was deleted and recreated during ExternalAllow reconciles (UID %s → %s): cross-reconciler deletion bug regressed",
			originalUID, after.UID)
	}
}

// TestIntegration_ApiserverReachable_CoexistsWithExtSvcPolicy_LBWebhookService
// covers the both-families-on-one-Service shape: a LoadBalancer Service
// that is ALSO referenced by a webhook config. Both ext.svc.* (ADR 0038)
// and ext.apiserver.* (ADR 0041) policies must coexist stably — each
// reconciler's sweep must leave the other family's policy alone.
func TestIntegration_ApiserverReachable_CoexistsWithExtSvcPolicy_LBWebhookService(t *testing.T) {
	ns := uniqueNS(t, "ar-lbwh")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeWebhookService(ns, "gateway")
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	mustCreate(t, svc)

	mustCreateValidatingWebhook(t, "ar-lbwh-"+ns, serviceWebhookClientConfig(ns, "gateway"))

	apiserverPol := waitForApiserverReachablePolicy(t, ns, "gateway", 10*time.Second)
	extSvcPol := waitForExternalAllowPolicy(t, ns, "gateway", 10*time.Second)
	apiserverUID := apiserverPol.UID
	extSvcUID := extSvcPol.UID

	// Trigger both reconcilers, then verify both policies survived
	// untouched (stable UIDs).
	updateService(t, ns, "gateway", func(s *corev1.Service) {
		metav1.SetMetaDataAnnotation(&s.ObjectMeta, "test-trigger", "coexist-check")
	})
	time.Sleep(2 * time.Second)

	var after networkingv1.NetworkPolicy
	if err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: apiserverReachablePolicyNameFor(ns, "gateway")}, &after); err != nil {
		t.Fatalf("ext.apiserver policy missing: %v", err)
	}
	if after.UID != apiserverUID {
		t.Errorf("ext.apiserver policy recreated (UID changed) — swept by the other family")
	}
	if err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "gateway")}, &after); err != nil {
		t.Fatalf("ext.svc policy missing: %v", err)
	}
	if after.UID != extSvcUID {
		t.Errorf("ext.svc policy recreated (UID changed) — swept by the other family")
	}
}
