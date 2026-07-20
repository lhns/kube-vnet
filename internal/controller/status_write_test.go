package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// updateStatus used to call r.Status().Update() unconditionally. Because the
// VirtualNetwork For() watch has no predicate, that write produced a watch
// event which re-enqueued the same vnet, which wrote again: a self-feeding
// loop costing one apiserver PUT plus (via the apply loop) one uncached
// NetworkPolicy GET per generated policy, indefinitely.
//
// ResourceVersion is the observable proof of an API write: the fake client
// bumps it on every accepted write and leaves it alone when we skip.

func newStatusReconciler(objs ...client.Object) (*VirtualNetworkReconciler, client.Client) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = vnetv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&vnetv1alpha1.VirtualNetwork{}).
		Build()
	return &VirtualNetworkReconciler{Client: c, Scheme: scheme, NSFilter: NewNamespaceFilter(nil)}, c
}

func rv(t *testing.T, c client.Client, ns, name string) string {
	t.Helper()
	var got vnetv1alpha1.VirtualNetwork
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get vnet: %v", err)
	}
	return got.ResourceVersion
}

// REGRESSION LOCK: identical input must not produce a second write.
func TestUpdateStatus_NoWriteWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
	}
	r, c := newStatusReconciler(vnet)

	members := map[string]map[Direction][]string{"home": {DirectionBoth: {"p1"}}}
	policies := []vnetv1alpha1.PolicyRef{{Namespace: "home", Name: "kube-vnet.mem.home.v-abc"}}

	// First call establishes status — this one must write.
	live := &vnetv1alpha1.VirtualNetwork{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, live); err != nil {
		t.Fatalf("get: %v", err)
	}
	stored1 := live.Status.DeepCopy() // as Reconcile does: before any set*()
	setReady(live, metav1.ConditionTrue, ReasonPoliciesGenerated, "1 policy")
	if err := r.updateStatus(ctx, live, members, policies, stored1); err != nil {
		t.Fatalf("first updateStatus: %v", err)
	}
	afterFirst := rv(t, c, "home", "v")

	// Second call with identical input must be a no-op.
	live2 := &vnetv1alpha1.VirtualNetwork{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, live2); err != nil {
		t.Fatalf("get: %v", err)
	}
	stored2 := live2.Status.DeepCopy()
	setReady(live2, metav1.ConditionTrue, ReasonPoliciesGenerated, "1 policy")
	if err := r.updateStatus(ctx, live2, members, policies, stored2); err != nil {
		t.Fatalf("second updateStatus: %v", err)
	}
	if got := rv(t, c, "home", "v"); got != afterFirst {
		t.Fatalf("status was rewritten with identical input (rv %s -> %s); "+
			"this re-enqueues the vnet and re-enters the reconcile loop", afterFirst, got)
	}
}

// The diff must not suppress a real change.
func TestUpdateStatus_WritesWhenChanged(t *testing.T) {
	ctx := context.Background()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
	}
	r, c := newStatusReconciler(vnet)

	base := map[string]map[Direction][]string{"home": {DirectionBoth: {"p1"}}}
	live := &vnetv1alpha1.VirtualNetwork{}
	_ = c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, live)
	if err := r.updateStatus(ctx, live, base, nil, live.Status.DeepCopy()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seeded := rv(t, c, "home", "v")

	for _, tc := range []struct {
		name    string
		members map[string]map[Direction][]string
		mutate  func(*vnetv1alpha1.VirtualNetwork)
	}{
		{
			name:    "member added",
			members: map[string]map[Direction][]string{"home": {DirectionBoth: {"p1", "p2"}}},
		},
		{
			name:    "condition reason changed",
			members: base,
			mutate: func(v *vnetv1alpha1.VirtualNetwork) {
				setDegraded(v, metav1.ConditionTrue, ReasonInvalidJoiners, "1 invalid joiner: home/bad:UnknownDirection")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := rv(t, c, "home", "v")
			cur := &vnetv1alpha1.VirtualNetwork{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, cur); err != nil {
				t.Fatalf("get: %v", err)
			}
			storedCur := cur.Status.DeepCopy() // before mutation, as Reconcile does
			if tc.mutate != nil {
				tc.mutate(cur)
			}
			if err := r.updateStatus(ctx, cur, tc.members, nil, storedCur); err != nil {
				t.Fatalf("updateStatus: %v", err)
			}
			if got := rv(t, c, "home", "v"); got == before {
				t.Fatalf("status was NOT written despite a real change (rv stayed %s)", before)
			}
		})
	}
	_ = seeded
}

// The status diff must not swallow Ready/Degraded transition Events. They are
// computed from the in-memory prior snapshot, independently of whether the
// write happened — this pins that decoupling.
func TestEmitTransitionEvents_IndependentOfStatusWrite(t *testing.T) {
	rec := &fakeRecorder{}
	r := &VirtualNetworkReconciler{Recorder: rec}

	v := &vnetv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"}}
	setReady(v, metav1.ConditionTrue, ReasonPoliciesGenerated, "ok")

	// prior=Unknown (never set) -> current=True is a transition.
	r.emitTransitionEvents(v, metav1.ConditionUnknown, metav1.ConditionUnknown)
	if len(rec.reasons) == 0 {
		t.Fatal("expected a Ready transition event even though no status write occurred")
	}
	found := false
	for _, x := range rec.reasons {
		if x == EventReady {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %q event, got %v", EventReady, rec.reasons)
	}
}

type fakeRecorder struct{ reasons []string }

func (f *fakeRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, reason, _, _ string, _ ...interface{}) {
	f.reasons = append(f.reasons, reason)
}

// REGRESSION LOCK for the bug this fix introduced and then fixed.
//
// The first implementation snapshotted the "prior" status *inside*
// updateStatus. But Reconcile calls setReady/setDegraded BEFORE updateStatus,
// and those mutate vnet.Status in place — so the snapshot captured the
// already-mutated value, compared it against itself, found no difference, and
// skipped EVERY write. Status froze permanently: Degraded never became True,
// and four integration tests failed.
//
// This pins the contract that makes the diff correct: the comparison baseline
// must be the status as fetched from the apiserver, captured before any
// condition mutation.
func TestUpdateStatus_WritesWhenConditionsMutatedBeforeCall(t *testing.T) {
	ctx := context.Background()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
	}
	r, c := newStatusReconciler(vnet)

	// Seed a stored status, exactly as a first reconcile would.
	seed := &vnetv1alpha1.VirtualNetwork{}
	_ = c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, seed)
	seedStored := seed.Status.DeepCopy()
	setDegraded(seed, metav1.ConditionFalse, ReasonNoIssues, "")
	if err := r.updateStatus(ctx, seed, nil, nil, seedStored); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := rv(t, c, "home", "v")

	// Second reconcile: fetch, snapshot, THEN mutate — the real ordering.
	cur := &vnetv1alpha1.VirtualNetwork{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, cur); err != nil {
		t.Fatalf("get: %v", err)
	}
	stored := cur.Status.DeepCopy()
	setDegraded(cur, metav1.ConditionTrue, ReasonInvalidJoiners, "1 invalid joiner: other/rogue:NamespaceNotAllowed")
	if err := r.updateStatus(ctx, cur, nil, nil, stored); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if got := rv(t, c, "home", "v"); got == before {
		t.Fatal("Degraded flipped False->True but no write happened; " +
			"the diff baseline must be the stored status, not the already-mutated object")
	}
	var final vnetv1alpha1.VirtualNetwork
	_ = c.Get(ctx, client.ObjectKey{Namespace: "home", Name: "v"}, &final)
	if s := conditionStatus(&final, "Degraded"); s != metav1.ConditionTrue {
		t.Fatalf("persisted Degraded = %v, want True", s)
	}
}
