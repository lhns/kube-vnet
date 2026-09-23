//go:build integration

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// extAllowPolicyName returns the policy name produced for Service ns/name,
// via the production builder so tests don't need to know the hash.
func extAllowPolicyName(ns, name string) string {
	return externalAllowPolicyName(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	})
}

// makeLBService is a minimal LB Service with selector {app: name}.
func makeLBService(ns, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func waitForExternalAllowPolicy(t *testing.T, ns, svcName string, timeout time.Duration) *networkingv1.NetworkPolicy {
	t.Helper()
	return waitForPolicy(t, ns, extAllowPolicyName(ns, svcName), timeout)
}

func waitForExternalAllowPolicyAbsent(t *testing.T, ns, svcName string, timeout time.Duration) {
	t.Helper()
	waitForPolicyAbsent(t, ns, extAllowPolicyName(ns, svcName), timeout)
}

func TestIntegration_ExternalAllow_LBServiceCreated_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "extallow-lb")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)

	pol := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
	if pol.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("missing managed-by label: %v", pol.Labels)
	}
	if pol.Labels[LabelRole] != LabelRoleExternalAllow {
		t.Errorf("wrong role label: %q", pol.Labels[LabelRole])
	}
	if pol.Labels[LabelSource] != "svc-web" {
		t.Errorf("source label: %q", pol.Labels[LabelSource])
	}
	if pol.Labels[LabelSourceKind] != LabelSourceKindService {
		t.Errorf("source-kind label: %q", pol.Labels[LabelSourceKind])
	}
	if got := pol.Spec.Ingress[0].From[0].IPBlock.CIDR; got != "0.0.0.0/0" {
		t.Errorf("ipBlock cidr = %q, want 0.0.0.0/0", got)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 80 {
		t.Errorf("port = %d, want 80", got)
	}
}

func TestIntegration_ExternalAllow_ServiceDeleted_PolicyCollected(t *testing.T) {
	ns := uniqueNS(t, "extallow-delsvc")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	if err := testClient.Delete(context.Background(), svc); err != nil {
		t.Fatalf("delete svc: %v", err)
	}
	// Removed by label on the NotFound path; envtest has no owner-ref GC.
	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_LBToClusterIP_PolicyRemoved(t *testing.T) {
	ns := uniqueNS(t, "extallow-flip")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	updateService(t, ns, "web", func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeClusterIP })
	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_SelectorChanged_PolicyUpdated(t *testing.T) {
	ns := uniqueNS(t, "extallow-selector")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	updateService(t, ns, "web", func(s *corev1.Service) { s.Spec.Selector = map[string]string{"app": "web-v2"} })

	eventually(t, 10*time.Second, func() error {
		pol, err := findPolicy(context.Background(), ns, extAllowPolicyName(ns, "web"))
		if err != nil {
			return err
		}
		if pol.Spec.PodSelector.MatchLabels["app"] != "web-v2" {
			return fmt.Errorf("podSelector not yet updated")
		}
		return nil
	})
}

func TestIntegration_ExternalAllow_PortsChanged_PolicyUpdated(t *testing.T) {
	ns := uniqueNS(t, "extallow-ports")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	updateService(t, ns, "web", func(s *corev1.Service) {
		s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{
			Name: "https", Port: 443, TargetPort: intstr.FromInt32(443), Protocol: corev1.ProtocolTCP,
		})
	})

	eventually(t, 10*time.Second, func() error {
		pol, err := findPolicy(context.Background(), ns, extAllowPolicyName(ns, "web"))
		if err != nil {
			return err
		}
		if len(pol.Spec.Ingress[0].Ports) != 2 {
			return fmt.Errorf("policy still has 1 port")
		}
		return nil
	})
}

func TestIntegration_ExternalAllow_DriftCorrection_PolicyDeleted(t *testing.T) {
	ns := uniqueNS(t, "extallow-drift-del")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	pol := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	if err := testClient.Delete(context.Background(), pol); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	eventually(t, 10*time.Second, func() error {
		p2, err := findPolicy(context.Background(), ns, extAllowPolicyName(ns, "web"))
		if err != nil {
			return err
		}
		if p2.UID == pol.UID {
			return fmt.Errorf("same UID, not yet recreated")
		}
		return nil
	})
}

func TestIntegration_ExternalAllow_AnnotationOptOut_ServiceLevel(t *testing.T) {
	ns := uniqueNS(t, "extallow-svc-opt")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	updateService(t, ns, "web", func(s *corev1.Service) {
		metav1.SetMetaDataAnnotation(&s.ObjectMeta, AnnotationExternalAllow, "false")
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_AnnotationOptOut_NSLevel(t *testing.T) {
	ns := uniqueNS(t, "extallow-ns-opt")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web-a"))
	mustCreate(t, makeLBService(ns, "web-b"))
	waitForExternalAllowPolicy(t, ns, "web-a", 10*time.Second)
	waitForExternalAllowPolicy(t, ns, "web-b", 10*time.Second)

	updateNamespace(t, ns, func(n *corev1.Namespace) {
		metav1.SetMetaDataAnnotation(&n.ObjectMeta, AnnotationExternalAllow, "false")
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web-a", 10*time.Second)
	waitForExternalAllowPolicyAbsent(t, ns, "web-b", 10*time.Second)
}

func TestIntegration_ExternalAllow_AnnotationOptOut_NSLevelFlip(t *testing.T) {
	ns := uniqueNS(t, "extallow-ns-flip")
	mustCreate(t, makeNamespace(ns, map[string]string{AnnotationExternalAllow: "false"}, nil))

	mustCreate(t, makeLBService(ns, "web"))
	assertPolicyStaysAbsent(t, ns, extAllowPolicyName(ns, "web"), 3*time.Second)

	// Removing the opt-out makes the policy appear.
	updateNamespace(t, ns, func(n *corev1.Namespace) { delete(n.Annotations, AnnotationExternalAllow) })

	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_DisabledNamespace_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "extallow-disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	mustCreate(t, makeLBService(ns, "web"))
	assertPolicyStaysAbsent(t, ns, extAllowPolicyName(ns, "web"), 3*time.Second)
}

func TestIntegration_ExternalAllow_OwnerReference(t *testing.T) {
	ns := uniqueNS(t, "extallow-ownerref")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))

	pol := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
	if len(pol.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner ref, got %d", len(pol.OwnerReferences))
	}
	ref := pol.OwnerReferences[0]
	if ref.Kind != "Service" || ref.Name != "web" {
		t.Errorf("ownerRef = %+v, want Service/web", ref)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Errorf("ownerRef.Controller should be true")
	}
}

func TestIntegration_ExternalAllow_NamedTargetPort_PendingThenReady(t *testing.T) {
	ns := uniqueNS(t, "extallow-namedport")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http")}}
	mustCreate(t, svc)

	// No backing pod yet, so the named port cannot be resolved.
	assertPolicyStaysAbsent(t, ns, extAllowPolicyName(ns, "web"), 3*time.Second)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-1", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "nginx",
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			}},
		},
	}
	mustCreate(t, pod)

	// The Pod watch re-enqueues the Service; this must not need the 30s requeue.
	pol := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8080 {
		t.Errorf("port = %d, want 8080 (resolved from named 'http')", got)
	}
}

// A rollout that renumbers a named containerPort: while old and new pods
// coexist both numbers are allowed, and deleting the last old pod drops the
// old number without any other event on the Service.
func TestIntegration_ExternalAllow_NamedTargetPort_Renumbered(t *testing.T) {
	ns := uniqueNS(t, "extallow-renumber")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http")}}
	mustCreate(t, svc)

	pod := func(name string, port int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": "web"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "main", Image: "nginx",
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			}}},
		}
	}
	waitForPorts := func(want ...int) {
		t.Helper()
		eventually(t, 10*time.Second, func() error {
			pol, err := findPolicy(context.Background(), ns, extAllowPolicyName(ns, "web"))
			if err != nil {
				return err
			}
			var got []int
			for _, p := range pol.Spec.Ingress[0].Ports {
				got = append(got, p.Port.IntValue())
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				return fmt.Errorf("ports = %v, want %v", got, want)
			}
			return nil
		})
	}

	old := pod("web-old", 8080)
	mustCreate(t, old)
	waitForPorts(8080)

	mustCreate(t, pod("web-new", 9090))
	waitForPorts(8080, 9090)

	if err := testClient.Delete(context.Background(), old); err != nil {
		t.Fatalf("delete old pod: %v", err)
	}
	waitForPorts(9090)

	// A relabel moves a pod into, then out of, the selector: each flip alone
	// must reach the Service.
	canary := pod("web-canary", 7070)
	canary.Labels = map[string]string{"app": "canary"}
	mustCreate(t, canary)
	relabel := func(app string) {
		t.Helper()
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"app":"`+app+`"}}}`))
		if err := testClient.Patch(context.Background(), canary, patch); err != nil {
			t.Fatalf("relabel canary: %v", err)
		}
	}
	relabel("web")
	waitForPorts(7070, 9090)
	relabel("canary")
	waitForPorts(9090)
}

// Setting kube-vnet/disabled on a namespace that already has policies removes
// them via the namespace watch.
func TestIntegration_ExternalAllow_NSDisabledMidFlight(t *testing.T) {
	ns := uniqueNS(t, "extallow-disabled-flip")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	updateNamespace(t, ns, func(n *corev1.Namespace) {
		metav1.SetMetaDataAnnotation(&n.ObjectMeta, "kube-vnet/disabled", "true")
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

// A Service that loses its selector (manually managed Endpoints) has nothing
// to scope a policy to, so the policy is removed.
func TestIntegration_ExternalAllow_SelectorClearedMidFlight(t *testing.T) {
	ns := uniqueNS(t, "extallow-selclear")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makeLBService(ns, "web"))
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	updateService(t, ns, "web", func(s *corev1.Service) { s.Spec.Selector = nil })

	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_NoSelector_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "extallow-nosel")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	svc.Spec.Selector = nil
	mustCreate(t, svc)

	assertPolicyStaysAbsent(t, ns, extAllowPolicyName(ns, "web"), 3*time.Second)
}

// Upgrade path from before ADR 0039: a legacy `kube-vnet.external-<svc>-<hash>`
// policy owned by the Service is swept once the new-format policy exists.
func TestIntegration_ExternalAllow_LegacyNameMigration(t *testing.T) {
	ns := uniqueNS(t, "extallow-legacy")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)

	// Wait for the first reconcile before planting the legacy policy, so the
	// sweep below is triggered by our annotation change and not by a race.
	newName := extAllowPolicyName(ns, "web")
	waitForPolicy(t, ns, newName, 10*time.Second)

	// Legacy label values: no LabelSourceKind, LabelSource is the bare name.
	truePtr := true
	legacy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vnet.external-web-deadbeef",
			Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
				LabelRole:      LabelRoleExternalAllow,
				LabelSource:    "web",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Service",
				Name: svc.Name, UID: svc.UID,
				Controller: &truePtr, BlockOwnerDeletion: &truePtr,
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	mustCreate(t, legacy)

	updateService(t, ns, "web", func(s *corev1.Service) {
		metav1.SetMetaDataAnnotation(&s.ObjectMeta, "test-trigger", "legacy-migration")
	})

	waitForPolicyAbsent(t, ns, legacy.Name, 10*time.Second)

	if _, err := findPolicy(context.Background(), ns, newName); err != nil {
		t.Errorf("new-format policy missing after migration: %v", err)
	}
}

// The reported shape: an LB on :80 with externalTrafficPolicy: Cluster. The
// policy must allow the targetPort, not a nodePort the apiserver assigns.
func TestIntegration_ExternalAllow_Regression_TraefikDaemonSet(t *testing.T) {
	ns := uniqueNS(t, "extallow-traefik")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "traefik"},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyTypeCluster,
			Selector:              map[string]string{"app.kubernetes.io/name": "traefik"},
			Ports: []corev1.ServicePort{{
				Name: "web", Port: 80, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	mustCreate(t, svc)

	pol := waitForExternalAllowPolicy(t, ns, "traefik", 10*time.Second)
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 80 {
		t.Errorf("traefik regression: port = %d, want 80 (targetPort)", got)
	}
	if got := pol.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"]; got != "traefik" {
		t.Errorf("podSelector match = %q, want traefik", got)
	}
}

// A Service name long enough to overflow the 63-char label-value limit once
// prefixed. Before SourceLabelValue bounded it, the apply was rejected with
// "metadata.labels: Invalid value" and the reconciler retried forever. Shape
// taken from a Helm-prefixed OpenTelemetry operator webhook Service.
//
// 61 chars: with the 4-char "svc-" prefix that is 65, over the limit. Keep it
// above 59 or the test stops testing anything.
const longSvcName = "opentelemetry-operator-opentelemetry-operator-webhook-service"

func TestIntegration_ExternalAllow_LongServiceName_PolicyStillApplies(t *testing.T) {
	ns := uniqueNS(t, "extallow-longname")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeLBService(ns, longSvcName))

	pol := waitForExternalAllowPolicy(t, ns, longSvcName, 15*time.Second)

	src := pol.Labels[LabelSource]
	if len(src) > 63 {
		t.Fatalf("source label is %d chars: %q", len(src), src)
	}
	if src != SourceLabelValue("svc-", ns, longSvcName) {
		t.Errorf("source label %q does not match SourceLabelValue; the delete "+
			"selector would not find this policy", src)
	}
}

// The label doubles as the List selector used to delete a Service's policies;
// if the written and queried values diverged, the delete would orphan it.
func TestIntegration_ExternalAllow_LongServiceName_DeleteRemovesPolicy(t *testing.T) {
	ns := uniqueNS(t, "extallow-longdel")
	mustCreate(t, makeNamespace(ns, nil, nil))
	svc := makeLBService(ns, longSvcName)
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, longSvcName, 15*time.Second)

	if err := testClient.Delete(context.Background(), svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	waitForExternalAllowPolicyAbsent(t, ns, longSvcName, 15*time.Second)
}

// The drift handler can't parse a truncated LabelSource back into a Service
// name, so it enqueues by owner reference. Deleting the policy must still
// bring it back.
func TestIntegration_ExternalAllow_LongServiceName_PolicyDriftRestored(t *testing.T) {
	ns := uniqueNS(t, "extallow-longdrift")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeLBService(ns, longSvcName))

	pol := waitForExternalAllowPolicy(t, ns, longSvcName, 15*time.Second)
	if err := testClient.Delete(context.Background(), pol); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	waitForExternalAllowPolicy(t, ns, longSvcName, 15*time.Second)
}
