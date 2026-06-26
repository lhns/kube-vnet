//go:build integration

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// extAllowPolicyName returns the policy name produced for a Service in `ns`
// with `name`. Matches the production builder so tests don't need to know
// the hash function.
func extAllowPolicyName(ns, name string) string {
	return externalAllowPolicyName(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	})
}

// makeLBService is a minimal LB Service factory. Selector defaults to
// {app: name}; tests override what they need.
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

// waitForExternalAllowPolicy polls until the policy for (ns, svcName) exists
// or the deadline expires.
func waitForExternalAllowPolicy(t *testing.T, ns, svcName string, timeout time.Duration) *networkingv1.NetworkPolicy {
	t.Helper()
	var pol networkingv1.NetworkPolicy
	eventually(t, timeout, func() error {
		return testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, svcName)}, &pol)
	})
	return &pol
}

// waitForExternalAllowPolicyAbsent polls until the policy for (ns, svcName)
// is gone or the deadline expires.
func waitForExternalAllowPolicyAbsent(t *testing.T, ns, svcName string, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, func() error {
		var pol networkingv1.NetworkPolicy
		err := testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, svcName)}, &pol)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return errPolicyStillExists
	})
}

var errPolicyStillExists = &simpleErr{"policy still exists"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// ---

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
	// Owner-ref cascade plus our explicit deletePolicyForService both apply
	// — either path is fine. Either way, the policy should be gone.
	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_LBToClusterIP_PolicyRemoved(t *testing.T) {
	ns := uniqueNS(t, "extallow-flip")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	// Flip type to plain ClusterIP. Reconciler should clean up.
	eventually(t, 5*time.Second, func() error {
		latest := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), latest); err != nil {
			return err
		}
		latest.Spec.Type = corev1.ServiceTypeClusterIP
		return testClient.Update(context.Background(), latest)
	})
	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_SelectorChanged_PolicyUpdated(t *testing.T) {
	ns := uniqueNS(t, "extallow-selector")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	// Change selector.
	eventually(t, 5*time.Second, func() error {
		latest := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), latest); err != nil {
			return err
		}
		latest.Spec.Selector = map[string]string{"app": "web-v2"}
		return testClient.Update(context.Background(), latest)
	})

	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		if err := testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &pol); err != nil {
			return err
		}
		if pol.Spec.PodSelector.MatchLabels["app"] != "web-v2" {
			return &simpleErr{"podSelector not yet updated"}
		}
		return nil
	})
}

func TestIntegration_ExternalAllow_PortsChanged_PolicyUpdated(t *testing.T) {
	ns := uniqueNS(t, "extallow-ports")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	eventually(t, 5*time.Second, func() error {
		latest := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), latest); err != nil {
			return err
		}
		latest.Spec.Ports = append(latest.Spec.Ports, corev1.ServicePort{
			Name: "https", Port: 443, TargetPort: intstr.FromInt32(443), Protocol: corev1.ProtocolTCP,
		})
		return testClient.Update(context.Background(), latest)
	})

	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		if err := testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &pol); err != nil {
			return err
		}
		if len(pol.Spec.Ingress[0].Ports) != 2 {
			return &simpleErr{"policy still has 1 port"}
		}
		return nil
	})
}

func TestIntegration_ExternalAllow_DriftCorrection_PolicyDeleted(t *testing.T) {
	ns := uniqueNS(t, "extallow-drift-del")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	pol := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	if err := testClient.Delete(context.Background(), pol); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	// Reconciler should re-create it.
	eventually(t, 10*time.Second, func() error {
		var p2 networkingv1.NetworkPolicy
		err := testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &p2)
		if err != nil {
			return err
		}
		if p2.UID == pol.UID {
			return &simpleErr{"same UID — not yet recreated"}
		}
		return nil
	})
}

func TestIntegration_ExternalAllow_AnnotationOptOut_ServiceLevel(t *testing.T) {
	ns := uniqueNS(t, "extallow-svc-opt")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	eventually(t, 5*time.Second, func() error {
		latest := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), latest); err != nil {
			return err
		}
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[AnnotationExternalAllow] = "false"
		return testClient.Update(context.Background(), latest)
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_AnnotationOptOut_NSLevel(t *testing.T) {
	ns := uniqueNS(t, "extallow-ns-opt")
	mustCreate(t, makeNamespace(ns, nil, nil))

	a := makeLBService(ns, "web-a")
	b := makeLBService(ns, "web-b")
	mustCreate(t, a)
	mustCreate(t, b)
	waitForExternalAllowPolicy(t, ns, "web-a", 10*time.Second)
	waitForExternalAllowPolicy(t, ns, "web-b", 10*time.Second)

	// Annotate NS opt-out → all external-allow policies in NS should go away.
	eventually(t, 5*time.Second, func() error {
		var nsObj corev1.Namespace
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: ns}, &nsObj); err != nil {
			return err
		}
		if nsObj.Annotations == nil {
			nsObj.Annotations = map[string]string{}
		}
		nsObj.Annotations[AnnotationExternalAllow] = "false"
		return testClient.Update(context.Background(), &nsObj)
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web-a", 10*time.Second)
	waitForExternalAllowPolicyAbsent(t, ns, "web-b", 10*time.Second)
}

func TestIntegration_ExternalAllow_AnnotationOptOut_NSLevelFlip(t *testing.T) {
	ns := uniqueNS(t, "extallow-ns-flip")
	mustCreate(t, makeNamespace(ns, map[string]string{AnnotationExternalAllow: "false"}, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)

	// Initially opted out — no policy should appear.
	// Wait a beat to let the reconciler run.
	time.Sleep(3 * time.Second)
	var pol networkingv1.NetworkPolicy
	if err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &pol); !apierrors.IsNotFound(err) {
		t.Fatalf("expected policy absent under NS opt-out, got err=%v", err)
	}

	// Remove the opt-out annotation. Policy should appear.
	eventually(t, 5*time.Second, func() error {
		var nsObj corev1.Namespace
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: ns}, &nsObj); err != nil {
			return err
		}
		delete(nsObj.Annotations, AnnotationExternalAllow)
		return testClient.Update(context.Background(), &nsObj)
	})

	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_DisabledNamespace_NoEmission(t *testing.T) {
	// Use the kube-vnet/disabled=true annotation (the canonical
	// per-namespace disable signal; equivalent to the flag-based
	// --disabled-namespaces for this reconciler's gate).
	ns := uniqueNS(t, "extallow-disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)

	// Wait, then assert nothing exists.
	time.Sleep(3 * time.Second)
	var pol networkingv1.NetworkPolicy
	err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &pol)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected policy absent in disabled NS, got err=%v, pol=%v", err, pol)
	}
}

func TestIntegration_ExternalAllow_OwnerReference(t *testing.T) {
	ns := uniqueNS(t, "extallow-ownerref")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)

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

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{
				Name: "http", Port: 80, TargetPort: intstr.FromString("http"),
			}},
		},
	}
	mustCreate(t, svc)

	// No backing pod yet — policy should NOT appear.
	time.Sleep(3 * time.Second)
	var pol networkingv1.NetworkPolicy
	err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &pol)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected policy absent before backing pod, got err=%v", err)
	}

	// Create a backing pod with the named port. Resolution should succeed
	// on next reconcile (triggered by the periodic requeue or by the
	// container-port lookup on subsequent Service event — accept either).
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

	// Pod Create watcher should re-enqueue this Service within ~1s
	// (no need to wait for the 30s safety-net requeue).
	pol2 := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
	if got := pol2.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8080 {
		t.Errorf("port = %d, want 8080 (resolved from named 'http')", got)
	}
}

func TestIntegration_ExternalAllow_NSDisabledMidFlight(t *testing.T) {
	// Verify that flipping kube-vnet/disabled=true on a managed NS
	// after policies exist deletes the external-allow policy via the
	// namespace watcher. The opt-out path mirrors NSFilter.IsManaged,
	// which is shared with the broader baseline + membership reconcilers.
	ns := uniqueNS(t, "extallow-disabled-flip")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	eventually(t, 5*time.Second, func() error {
		var nsObj corev1.Namespace
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: ns}, &nsObj); err != nil {
			return err
		}
		if nsObj.Annotations == nil {
			nsObj.Annotations = map[string]string{}
		}
		nsObj.Annotations["kube-vnet/disabled"] = "true"
		return testClient.Update(context.Background(), &nsObj)
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_SelectorClearedMidFlight(t *testing.T) {
	// A Service that loses its selector (transitioning to
	// manually-managed Endpoints) should have its policy cleaned up —
	// nothing to scope to anymore.
	ns := uniqueNS(t, "extallow-selclear")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	mustCreate(t, svc)
	waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)

	eventually(t, 5*time.Second, func() error {
		latest := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), latest); err != nil {
			return err
		}
		latest.Spec.Selector = nil
		return testClient.Update(context.Background(), latest)
	})

	waitForExternalAllowPolicyAbsent(t, ns, "web", 10*time.Second)
}

func TestIntegration_ExternalAllow_NoSelector_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "extallow-nosel")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{
				Port: 80, TargetPort: intstr.FromInt32(80),
			}},
			// Selector intentionally nil — manually-managed Endpoints.
		},
	}
	mustCreate(t, svc)

	time.Sleep(3 * time.Second)
	var pol networkingv1.NetworkPolicy
	err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: extAllowPolicyName(ns, "web")}, &pol)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected policy absent for no-selector svc, got err=%v", err)
	}
}

func TestIntegration_ExternalAllow_LegacyNameMigration(t *testing.T) {
	// Reproduces the user-observed state: an operator was running an old
	// version that emitted `kube-vnet.external-<svc>-<hash>` policies
	// (pre-ADR-0039). After upgrade, the new operator's reconcile should
	// emit the new `kube-vnet.ext.svc.<svc>-<hash>` policy AND clean up
	// the legacy one via the owner-ref-based sweep.
	ns := uniqueNS(t, "extallow-legacy")
	mustCreate(t, makeNamespace(ns, nil, nil))

	// Create Service first so we have a UID to point the legacy
	// OwnerReference at.
	svc := makeLBService(ns, "web")
	mustCreate(t, svc)

	// Wait for the new-format policy to appear (proves the reconciler
	// ran). We need this before pre-creating the legacy policy because
	// otherwise the reconciler might run between the legacy create and
	// our wait.
	newName := extAllowPolicyName(ns, "web")
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		return testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: newName}, &pol)
	})

	// Now plant a legacy-format policy alongside the new one. Carries
	// the old label values: no LabelSourceKind, LabelSource is the bare
	// service name. Owner-ref points at the real Service.
	truePtr := true
	var fetchedSvc corev1.Service
	if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), &fetchedSvc); err != nil {
		t.Fatalf("get svc: %v", err)
	}
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
				Name: fetchedSvc.Name, UID: fetchedSvc.UID,
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

	// Trigger a reconcile of the Service via an inert annotation flip.
	eventually(t, 5*time.Second, func() error {
		latest := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(svc), latest); err != nil {
			return err
		}
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations["test-trigger"] = "legacy-migration"
		return testClient.Update(context.Background(), latest)
	})

	// Within one reconcile cycle the legacy policy should be gone.
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		err := testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "kube-vnet.external-web-deadbeef"}, &pol)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return &simpleErr{"legacy policy still exists"}
	})

	// And the new one should still be there.
	var newPol networkingv1.NetworkPolicy
	if err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: newName}, &newPol); err != nil {
		t.Errorf("new-format policy missing after migration: %v", err)
	}
}

func TestIntegration_ExternalAllow_Regression_TraefikDaemonSet(t *testing.T) {
	// Mirror of the user's actual hit: LB on :80, externalTrafficPolicy:Cluster,
	// backed by a DaemonSet-style pod set. The emitted policy must allow on
	// port=80 (targetPort), not on any nodePort the apiserver might assign.
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
