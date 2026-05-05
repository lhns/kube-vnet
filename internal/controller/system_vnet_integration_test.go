//go:build integration

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// TestIntegration_SystemVnet_NamespaceCreation: a managed namespace gets a
// `namespace` system VirtualNetwork stamped with kube-vnet/system=true.
func TestIntegration_SystemVnet_NamespaceCreation(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "sys-ns-create")
	mustCreate(t, makeNamespace(ns, nil, nil))

	v := &vnetv1alpha1.VirtualNetwork{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemVnetNamespace}, v)
	})

	if got := v.Labels[LabelSystem]; got != LabelSystemValue {
		t.Errorf("system label = %q, want %q", got, LabelSystemValue)
	}
	if got := v.Labels[LabelManagedBy]; got != LabelManagedByValue {
		t.Errorf("managed-by label = %q, want %q", got, LabelManagedByValue)
	}
}

// TestIntegration_SystemVnet_DriftCorrection: deleting the namespace system
// vnet causes the operator to recreate it.
func TestIntegration_SystemVnet_DriftCorrection(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "sys-drift")
	mustCreate(t, makeNamespace(ns, nil, nil))

	v := &vnetv1alpha1.VirtualNetwork{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemVnetNamespace}, v)
	})

	if err := testClient.Delete(ctx, v); err != nil {
		t.Fatalf("delete system vnet: %v", err)
	}

	// Wait for it to come back.
	eventually(t, 10*time.Second, func() error {
		v2 := &vnetv1alpha1.VirtualNetwork{}
		err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemVnetNamespace}, v2)
		if apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
}
