//go:build integration

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func setOperatorDefaults(t *testing.T, defs []OperatorMembership) {
	t.Helper()
	prior := append([]OperatorMembership(nil), testResolutionDefaults...)
	testResolutionReconciler.OperatorDefaults = defs
	testResolutionDefaults = defs
	t.Cleanup(func() {
		testResolutionReconciler.OperatorDefaults = prior
		testResolutionDefaults = prior
	})
}

// TestIntegration_Resolution_OperatorDefaultsStamped: with operator defaults
// `[namespace=both, cluster=egress]`, a pod in a managed namespace gets
// kube-vnet.system/net.namespace=both and kube-vnet.system/net.cluster=egress
// stamped, plus the resolved-generation annotation.
func TestIntegration_Resolution_OperatorDefaultsStamped(t *testing.T) {
	setOperatorDefaults(t, []OperatorMembership{
		{Vnet: "namespace", Direction: DirectionBoth},
		{Vnet: "cluster", Direction: DirectionEgress},
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-defaults")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "x"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.namespace"]; got != "both" {
			return fmt.Errorf("namespace label = %q, want both; labels=%v", got, p.Labels)
		}
		if got := p.Labels["kube-vnet.system/net.cluster"]; got != "egress" {
			return fmt.Errorf("cluster label = %q, want egress; labels=%v", got, p.Labels)
		}
		if p.Annotations[AnnotationResolvedGeneration] == "" {
			return fmt.Errorf("resolved-generation annotation missing")
		}
		return nil
	})
}

// TestIntegration_Resolution_PodLabelOverridesDefault: pod-authored
// kube-vnet/net.cluster=both wins over operator default cluster=egress.
func TestIntegration_Resolution_PodLabelOverridesDefault(t *testing.T) {
	setOperatorDefaults(t, []OperatorMembership{
		{Vnet: "cluster", Direction: DirectionEgress},
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-override")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.cluster": "both"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.cluster"]; got != "both" {
			return fmt.Errorf("cluster label = %q, want both (pod-label override)", got)
		}
		return nil
	})
}

// TestIntegration_Resolution_NoneOptsOut: pod-authored kube-vnet/net.namespace=none
// strips the inherited operator-default membership; no kube-vnet.system label
// for `namespace` ends up on the pod.
func TestIntegration_Resolution_NoneOptsOut(t *testing.T) {
	setOperatorDefaults(t, []OperatorMembership{
		{Vnet: "namespace", Direction: DirectionBoth},
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-optout")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.namespace": "none"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if _, ok := p.Labels["kube-vnet.system/net.namespace"]; ok {
			return fmt.Errorf("namespace label should be absent (=none opt-out), got labels=%v", p.Labels)
		}
		return nil
	})
}

// TestIntegration_Resolution_VirtualNetworkBindingStamped: a VNB with
// podSelector matching this pod stamps the system label.
func TestIntegration_Resolution_VirtualNetworkBindingStamped(t *testing.T) {
	setOperatorDefaults(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-vnb")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: ns},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-binding", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "payments", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		// Bare form because pod and vnet share the same namespace.
		if got := p.Labels["kube-vnet.system/net.payments"]; got != "both" {
			return fmt.Errorf("payments label = %q, want both", got)
		}
		return nil
	})
}

// TestIntegration_Resolution_BindingConflictIntersection: two
// VirtualNetworkBindings in the same NS, with overlapping podSelectors and
// disagreeing directions. Per ADR 0031 the conflict resolves via
// intersection (fail-closed): both ∩ ingress = ingress.
func TestIntegration_Resolution_BindingConflictIntersection(t *testing.T) {
	setOperatorDefaults(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-conflict")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})

	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "b-deny", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "ingress",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "a-allow", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.v"]; got != "ingress" {
			return fmt.Errorf("v label = %q, want ingress (intersection of both + ingress per ADR 0031); labels=%v", got, p.Labels)
		}
		return nil
	})
}

// TestIntegration_Resolution_ClusterBindingNamespaceSelector: a CVNB with a
// namespaceSelector matches pods only in the matching namespaces.
func TestIntegration_Resolution_ClusterBindingNamespaceSelector(t *testing.T) {
	setOperatorDefaults(t, nil)

	ctx := context.Background()
	matched := uniqueNS(t, "res-cvnb-match")
	unmatched := uniqueNS(t, "res-cvnb-skip")
	mustCreate(t, makeNamespace(matched, nil, map[string]string{"tier": "prod"}))
	mustCreate(t, makeNamespace(unmatched, nil, map[string]string{"tier": "dev"}))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: matched},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{All: true},
		},
	})
	cvnb := &vnetv1alpha1.ClusterVirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueNS(t, "prod-only-cvnb")},
		Spec: vnetv1alpha1.ClusterVirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "shared", Namespace: matched},
			Direction:         "both",
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			PodSelector:       metav1.LabelSelector{},
		},
	}
	mustCreate(t, cvnb)
	t.Cleanup(func() { _ = testClient.Delete(ctx, cvnb) })

	mustCreate(t, makePod(matched, "p1", map[string]string{"app": "x"}))
	mustCreate(t, makePod(unmatched, "p2", map[string]string{"app": "x"}))

	// Matched-NS pod is bare-form because its namespace is the vnet's home.
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: matched, Name: "p1"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.shared"]; got != "both" {
			return fmt.Errorf("matched-NS pod missing label: %v", p.Labels)
		}
		return nil
	})

	// Unmatched-NS pod must not get the system label (foreign-NS would use
	// the prefixed form `<homeNS>.shared`; check both forms are absent).
	time.Sleep(2 * time.Second)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: unmatched, Name: "p2"}, p); err != nil {
		t.Fatalf("get unmatched pod: %v", err)
	}
	prefixedKey := "kube-vnet.system/net." + matched + ".shared"
	if _, ok := p.Labels["kube-vnet.system/net.shared"]; ok {
		t.Errorf("unmatched-NS pod should not have bare label, got %v", p.Labels)
	}
	if _, ok := p.Labels[prefixedKey]; ok {
		t.Errorf("unmatched-NS pod should not have prefixed label %q, got %v", prefixedKey, p.Labels)
	}
}

// TestIntegration_Resolution_BindingDeletionStripsLabel: when a VNB is
// deleted, the system labels it caused to be stamped are removed from the
// affected pods.
func TestIntegration_Resolution_BindingDeletionStripsLabel(t *testing.T) {
	setOperatorDefaults(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-bdel")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	binding := &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "stamper", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	}
	mustCreate(t, binding)
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	// Wait for the binding to stamp the label.
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.v"]; got != "both" {
			return fmt.Errorf("not yet stamped: %v", p.Labels)
		}
		return nil
	})

	// Delete the binding; the label should be removed.
	if err := testClient.Delete(ctx, binding); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if _, ok := p.Labels["kube-vnet.system/net.v"]; ok {
			return fmt.Errorf("label should have been stripped, got %v", p.Labels)
		}
		return nil
	})
}
