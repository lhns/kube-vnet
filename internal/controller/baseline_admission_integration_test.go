//go:build integration

package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// CRD-schema-level CEL validation tests (ADR 0031). These don't need the
// VAP-impersonation pattern from chart_vap_admission_integration_test.go —
// they fire on every Create regardless of user.

func TestIntegration_Baseline_ClusterSingletonRejected(t *testing.T) {
	ctx := context.Background()
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "not-default"},
	}
	err := testClient.Create(ctx, cb)
	if err == nil {
		_ = testClient.Delete(ctx, cb)
		t.Fatalf("apiserver accepted ClusterVirtualNetworkBaseline named 'not-default'; singleton CEL not enforcing")
	}
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "default") {
		t.Fatalf("expected Invalid w/ singleton message, got: %v", err)
	}
}

func TestIntegration_Baseline_NamespaceSingletonRejected(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "baseline-singleton")
	mustCreate(t, makeNamespace(ns, nil, nil))

	nb := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "not-default", Namespace: ns},
	}
	err := testClient.Create(ctx, nb)
	if err == nil {
		_ = testClient.Delete(ctx, nb)
		t.Fatalf("apiserver accepted VirtualNetworkBaseline named 'not-default'; singleton CEL not enforcing")
	}
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "default") {
		t.Fatalf("expected Invalid w/ singleton message, got: %v", err)
	}
}

func TestIntegration_Binding_EmptyPodSelectorRejected(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "binding-empty")
	mustCreate(t, makeNamespace(ns, nil, nil))

	b := &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{},
		},
	}
	err := testClient.Create(ctx, b)
	if err == nil {
		_ = testClient.Delete(ctx, b)
		t.Fatalf("apiserver accepted VirtualNetworkBinding with empty podSelector; CEL not enforcing")
	}
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "podSelector") {
		t.Fatalf("expected Invalid w/ podSelector message, got: %v", err)
	}
}

func TestIntegration_Binding_DefaultDirectionRejected(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "binding-defaultdir")
	mustCreate(t, makeNamespace(ns, nil, nil))

	b := &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-dir", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "default-both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	}
	err := testClient.Create(ctx, b)
	if err == nil {
		_ = testClient.Delete(ctx, b)
		t.Fatalf("apiserver accepted VirtualNetworkBinding with direction=default-both; enum CEL not enforcing")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected Invalid, got: %v", err)
	}
}
