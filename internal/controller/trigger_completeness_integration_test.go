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

// These tests all pin one invariant (ADR 0044): a reconciler's trigger set must
// cover its read set. If a controller decides something by reading state it
// does not watch, that decision is never revisited.
//
// It does not merely go stale for a while — it is permanent. Every predicate in
// this operator is change-based (a deliberate churn fix, 75c14a6), and the
// informer resync delivers Update events where old == new, so resync is
// filtered out. Before the churn work, membership predicates fired on every pod
// heartbeat, which accidentally re-ran these decisions often enough to hide the
// gaps.
//
// Each test below grants or restores access WITHOUT touching the object whose
// state must change, and asserts it converges anyway.

// settleQuiet is long enough for every self-generated event to drain.
//
// This matters more than it looks: a vnet whose home namespace is excluded
// deletes its own membership policy, and that delete event re-enqueues the vnet
// via policyToVNet. Re-enabling the namespace while that is still in flight
// measures the race instead of the steady state — an earlier version of the
// home-namespace test below passed for exactly that reason.
const settleQuiet = 15 * time.Second

// Bug #2. allowedNamespaces.selector matches namespaces by LABEL, but the
// resolution controller's Namespace watch filtered to AnnotationChangedPredicate.
// Labelling a namespace to grant it access is the documented workflow, and it
// never took effect.
func TestIntegration_Trigger_NamespaceLabelGrantsAccessLater(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	home := uniqueNS(t, "tc-lbl-home")
	member := uniqueNS(t, "tc-lbl-member")
	mustCreate(t, makeNamespace(home, nil, nil))
	// Deliberately created WITHOUT tier=prod: not yet permitted.
	mustCreate(t, makeNamespace(member, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "sel", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		},
	})
	mustCreate(t, makePod(member, "p", map[string]string{
		"kube-vnet/net." + home + ".sel": "both",
	}))

	sysLabel := "kube-vnet.system/net." + home + ".sel"

	time.Sleep(settleQuiet)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: member, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels[sysLabel]; ok {
		t.Fatalf("pod stamped %q while its namespace was not yet permitted", got)
	}

	// Grant access by LABELLING the namespace. The pod is never touched.
	ns := &corev1.Namespace{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: member}, ns); err != nil {
		t.Fatalf("get ns: %v", err)
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["tier"] = "prod"
	if err := testClient.Update(ctx, ns); err != nil {
		t.Fatalf("update ns: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: member, Name: "p"}, p); err != nil {
			return err
		}
		if p.Labels[sysLabel] != "both" {
			return fmt.Errorf("pod not stamped after its namespace was granted access")
		}
		return nil
	})
}

// Control for the test above: the same grant via the disabled ANNOTATION, which
// the watch already observed. It isolates the label path — if this one ever
// fails too, the problem is namespace events in general, not the predicate.
func TestIntegration_Trigger_NamespaceAnnotationGrantIsTheControl(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	ns := uniqueNS(t, "tc-ann")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.c": "both"}))

	sysLabel := "kube-vnet.system/net." + ns + ".c"
	time.Sleep(settleQuiet)

	n := &corev1.Namespace{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: ns}, n); err != nil {
		t.Fatalf("get ns: %v", err)
	}
	delete(n.Annotations, "kube-vnet/disabled")
	if err := testClient.Update(ctx, n); err != nil {
		t.Fatalf("update ns: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if p.Labels[sysLabel] != "both" {
			return fmt.Errorf("pod not stamped after the namespace was re-enabled")
		}
		return nil
	})
}

// Bug #3. The VirtualNetworkReconciler reads namespace managed-ness but did not
// watch Namespace, and its excluded path returns without the 10-minute requeue
// the happy path uses. Re-enabling the home namespace left the vnet Degraded
// with its membership policies deleted, so its members stayed isolated by the
// deny-all baseline.
//
// The members live in a DIFFERENT namespace on purpose: that is the shared-vnet
// shape, and it defeats the indirect rescue where re-resolving the home
// namespace's own pods happens to wake the vnet.
func TestIntegration_Trigger_HomeNamespaceReEnabledRecoversVnet(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	home := uniqueNS(t, "tc-home")
	member := uniqueNS(t, "tc-member")
	mustCreate(t, makeNamespace(home, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, makeNamespace(member, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{member}},
		},
	})
	mustCreate(t, makePod(member, "p", map[string]string{
		"kube-vnet/net." + home + ".v": "both",
	}))

	eventually(t, 20*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "v"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") != metav1.ConditionTrue {
			return fmt.Errorf("expected Degraded=True while the home namespace is disabled")
		}
		return nil
	})

	// Required — see settleQuiet.
	time.Sleep(settleQuiet)

	n := &corev1.Namespace{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: home}, n); err != nil {
		t.Fatalf("get ns: %v", err)
	}
	delete(n.Annotations, "kube-vnet/disabled")
	if err := testClient.Update(ctx, n); err != nil {
		t.Fatalf("update ns: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "v"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") == metav1.ConditionTrue {
			return fmt.Errorf("vnet still Degraded after its home namespace was re-enabled")
		}
		if _, err := findPolicy(ctx, member, PolicyName("v", home)); err != nil {
			return fmt.Errorf("membership policy not restored: %w", err)
		}
		return nil
	})
}

// Bug #4. The binding reconciler computes status.attachedPods by listing pods,
// but watched neither Pod nor Namespace and has no requeue, so its status froze
// at whatever was true when the binding was last reconciled.
//
// Status-only: membership stamping is the resolution controller's job and it
// does watch Pod. What breaks is `kubectl get vnb` telling the truth.
func TestIntegration_Trigger_PodCreatedAfterBindingUpdatesStatus(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	ns := uniqueNS(t, "tc-vnb-pod")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})

	// Binding settles with no pods matching.
	time.Sleep(settleQuiet)
	b := &vnetv1alpha1.VirtualNetworkBinding{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "b"}, b); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if len(b.Status.AttachedPods) != 0 {
		t.Fatalf("expected no attached pods yet, got %v", b.Status.AttachedPods)
	}

	// The pod appears afterwards. The binding is never touched.
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	eventually(t, 30*time.Second, func() error {
		b := &vnetv1alpha1.VirtualNetworkBinding{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "b"}, b); err != nil {
			return err
		}
		for _, p := range b.Status.AttachedPods {
			if p == "p" {
				return nil
			}
		}
		return fmt.Errorf("attachedPods = %v, want it to contain \"p\"", b.Status.AttachedPods)
	})
}

// Bug #4, namespace half: the binding also reads its namespace's managed-ness.
func TestIntegration_Trigger_BindingNamespaceReEnabledRecoversStatus(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	ns := uniqueNS(t, "tc-vnb-ns")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})

	eventually(t, 20*time.Second, func() error {
		b := &vnetv1alpha1.VirtualNetworkBinding{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "b"}, b); err != nil {
			return err
		}
		if bindingReadyReason(b) != ReasonBindingNamespaceExcluded {
			return fmt.Errorf("reason = %q, want %q", bindingReadyReason(b), ReasonBindingNamespaceExcluded)
		}
		return nil
	})

	time.Sleep(settleQuiet)

	n := &corev1.Namespace{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: ns}, n); err != nil {
		t.Fatalf("get ns: %v", err)
	}
	delete(n.Annotations, "kube-vnet/disabled")
	if err := testClient.Update(ctx, n); err != nil {
		t.Fatalf("update ns: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		b := &vnetv1alpha1.VirtualNetworkBinding{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "b"}, b); err != nil {
			return err
		}
		if r := bindingReadyReason(b); r == ReasonBindingNamespaceExcluded {
			return fmt.Errorf("binding still reports %q after its namespace was re-enabled", r)
		}
		return nil
	})
}

func bindingReadyReason(b *vnetv1alpha1.VirtualNetworkBinding) string {
	for _, c := range b.Status.Conditions {
		if c.Type == "Ready" {
			return c.Reason
		}
	}
	return ""
}
