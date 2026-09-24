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

// TestIntegration_TerminatingMemberNamespace_DoesNotBlockOthers: a member
// namespace being deleted must not stall the vnet. The namespace controller
// deletes a terminating namespace's NetworkPolicies while its pods are still
// shutting down; the vnet reconcile then tried to recreate the policy,
// NamespaceLifecycle admission rejected the create, and the reconcile
// aborted before applying any later namespace's policy, set Ready=False
// (ApplyFailed) and retried with backoff until the namespace was gone.
//
// envtest runs no namespace controller, so a deleted namespace stays
// Terminating for the rest of the run, which is what this test needs.
func TestIntegration_TerminatingMemberNamespace_DoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	// Generate emits policies sorted by namespace; "vta-" sorts the
	// terminating namespace before the live one, so an aborting apply loop
	// never reaches the live namespace.
	term := uniqueNS(t, "vta-term")
	live := uniqueNS(t, "vtb-live")
	home := uniqueNS(t, "vtc-home")
	for _, ns := range []string{term, live, home} {
		mustCreate(t, makeNamespace(ns, nil, nil))
	}
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{term, live}},
		},
	})
	joinFQ := map[string]string{"kube-vnet/net." + home + ".v": "both"}
	mustCreate(t, makePod(home, "h", map[string]string{"kube-vnet/net.v": "both"}))
	mustCreate(t, makePod(term, "t", joinFQ))

	policyName := PolicyName("v", home)
	waitForPolicy(t, term, policyName, 10*time.Second)

	// Start deleting the namespace, then do what the namespace controller
	// would: remove its content. The pod stays (still shutting down).
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: term}}
	if err := testClient.Delete(ctx, nsObj); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	eventually(t, 10*time.Second, func() error {
		if err := testClient.Get(ctx, client.ObjectKey{Name: term}, nsObj); err != nil {
			return err
		}
		if nsObj.DeletionTimestamp == nil {
			return fmt.Errorf("namespace %s not terminating yet", term)
		}
		return nil
	})
	pol, err := findPolicy(ctx, term, policyName)
	if err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if err := testClient.Delete(ctx, pol); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	// A new member elsewhere must still get its policy, and the vnet must
	// stay Ready.
	mustCreate(t, makePod(live, "l", joinFQ))
	waitForPolicy(t, live, policyName, 10*time.Second)
	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "v"}, v); err != nil {
			return err
		}
		if got := conditionReason(v.Status.Conditions, "Ready"); conditionStatus(v, "Ready") != metav1.ConditionTrue {
			return fmt.Errorf("Ready not True (reason %q)", got)
		}
		// The terminating namespace's pods still run until they are gone,
		// so they stay members.
		var nss []string
		for _, m := range v.Status.Members {
			nss = append(nss, m.Namespace)
		}
		if len(nss) != 3 {
			return fmt.Errorf("member namespaces = %v, want %s, %s and %s", nss, term, live, home)
		}
		return nil
	})

	// Nothing tried to recreate the policy in the terminating namespace.
	assertPolicyStaysAbsent(t, term, policyName, 2*time.Second)
	var evs corev1.EventList
	if err := testClient.List(ctx, &evs, client.InNamespace(home)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range evs.Items {
		if e.Reason == EventApplyFailed {
			t.Errorf("ApplyFailed event: %s", e.Message)
		}
	}
}
