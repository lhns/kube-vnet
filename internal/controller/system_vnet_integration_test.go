//go:build integration

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// TestIntegration_SystemVnet_NamespaceCreation: a managed namespace gets a
// `namespace` system VirtualNetwork stamped with kube-vnet.system/managed-by=kube-vnet.
func TestIntegration_SystemVnet_NamespaceCreation(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "sys-ns-create")
	mustCreate(t, makeNamespace(ns, nil, nil))

	v := &vnetv1alpha1.VirtualNetwork{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemVnetNamespace}, v)
	})

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

// TestIntegration_SystemVnet_HomeNamespaceExcluded_StillReady verifies that
// a system-labeled vnet whose home namespace is disabled (kube-vnet/disabled=
// true) is NOT marked Degraded with ReasonHomeNamespaceExcluded. The cluster
// system vnet lives in the operator namespace, which the operator implicitly
// adds to disabledNamespaces as a privilege boundary; without this exemption
// the cluster vnet would never reach a usable state on a fresh install.
//
// Sibling assertion: a user-authored vnet (no system label) in the same
// disabled namespace still trips the home-namespace-excluded path. This
// proves the short-circuit is gated on the label, not on the name.
func TestIntegration_SystemVnet_HomeNamespaceExcluded_StillReady(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "sys-disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	sysVnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SystemVnetCluster,
			Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
	}
	mustCreate(t, sysVnet)

	userVnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "userv", Namespace: ns},
	}
	mustCreate(t, userVnet)

	// System vnet must NOT end up with HomeNamespaceExcluded.
	eventually(t, 10*time.Second, func() error {
		got := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemVnetCluster}, got); err != nil {
			return err
		}
		if reason := conditionReasonOf(got, "Ready"); reason == ReasonHomeNamespaceExcluded {
			return fmt.Errorf("system vnet Ready reason is %q, want anything except %q", reason, ReasonHomeNamespaceExcluded)
		}
		if reason := conditionReasonOf(got, "Degraded"); reason == ReasonHomeNamespaceExcluded {
			return fmt.Errorf("system vnet Degraded reason is %q, want anything except %q", reason, ReasonHomeNamespaceExcluded)
		}
		// Need at least one condition set to know the reconciler has touched it
		// (otherwise we might be observing the pre-reconcile state).
		if len(got.Status.Conditions) == 0 {
			return fmt.Errorf("no conditions yet")
		}
		return nil
	})

	// Sibling user vnet must still trip HomeNamespaceExcluded.
	eventually(t, 10*time.Second, func() error {
		got := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "userv"}, got); err != nil {
			return err
		}
		if reason := conditionReasonOf(got, "Degraded"); reason != ReasonHomeNamespaceExcluded {
			return fmt.Errorf("user vnet Degraded reason is %q, want %q", reason, ReasonHomeNamespaceExcluded)
		}
		return nil
	})
}

func conditionReasonOf(vnet *vnetv1alpha1.VirtualNetwork, t string) string {
	for _, c := range vnet.Status.Conditions {
		if c.Type == t {
			return c.Reason
		}
	}
	return ""
}
