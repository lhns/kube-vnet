package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/testutil"
)

// patchCountingClient returns a fake client holding objs that supports
// server-side apply, and a pointer to its Patch call count. failNS, if set,
// makes every Patch in that namespace fail with errInjected.
func patchCountingClient(t *testing.T, failNS string, objs ...client.Object) (client.Client, *int) {
	t.Helper()
	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(testutil.Scheme(t, networkingv1.AddToScheme)).
		WithObjects(objs...).
		WithStatusSubresource(&vnetv1alpha1.VirtualNetwork{}).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if obj.GetNamespace() == failNS {
					return errInjected
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	return c, &patches
}

// applyPolicy writes only when the live policy differs from the desired one
// on something the apply sets.
func TestApplyPolicy_SkipsNoOpApplies(t *testing.T) {
	ctx := context.Background()
	c, patches := patchCountingClient(t, "")
	apply := func() bool {
		t.Helper()
		created, err := applyPolicy(ctx, c, c, DesiredBaseline("ns"))
		if err != nil {
			t.Fatalf("applyPolicy: %v", err)
		}
		return created
	}
	live := func() *networkingv1.NetworkPolicy {
		t.Helper()
		p := &networkingv1.NetworkPolicy{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: BaselinePolicyName}, p); err != nil {
			t.Fatalf("get: %v", err)
		}
		return p
	}

	// Missing: created.
	if !apply() || *patches != 1 {
		t.Fatalf("missing policy: want a create (1 patch), got %d patches", *patches)
	}
	// Equal: no write.
	if apply() || *patches != 1 {
		t.Fatalf("up-to-date policy: want no patch, got %d patches", *patches)
	}

	// A foreign annotation is not the apply's to change: still no write.
	p := live()
	p.Annotations = map[string]string{"example.com/note": "x"}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if apply(); *patches != 1 {
		t.Fatalf("foreign annotation: want no patch, got %d patches", *patches)
	}

	// Spec drift: re-applied and corrected.
	p = live()
	p.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if apply(); *patches != 2 {
		t.Fatalf("spec drift: want a patch, got %d patches", *patches)
	}
	if got := live().Spec.PolicyTypes; len(got) != 1 {
		t.Fatalf("drift not corrected: policyTypes = %v", got)
	}

	// Label drift: re-applied.
	p = live()
	delete(p.Labels, LabelRole)
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if apply(); *patches != 3 {
		t.Fatalf("label drift: want a patch, got %d patches", *patches)
	}
	if live().Labels[LabelRole] != LabelRoleBaseline {
		t.Fatal("label drift not corrected")
	}
}

func TestPolicyUpToDate(t *testing.T) {
	desired := func() *networkingv1.NetworkPolicy {
		return buildHostPortPolicy("ns", hostPortKey{port: 8080, protocol: corev1.ProtocolTCP})
	}
	now := metav1.Now()
	for _, tc := range []struct {
		name   string
		mutate func(live *networkingv1.NetworkPolicy)
		want   bool
	}{
		{"equal", func(*networkingv1.NetworkPolicy) {}, true},
		{"server fields differ", func(l *networkingv1.NetworkPolicy) {
			l.ResourceVersion, l.UID, l.Generation = "7", "uid", 3
		}, true},
		{"extra label", func(l *networkingv1.NetworkPolicy) { l.Labels["x"] = "y" }, false},
		{"owner ref added", func(l *networkingv1.NetworkPolicy) {
			l.OwnerReferences = []metav1.OwnerReference{{Kind: "Service", Name: "s"}}
		}, false},
		{"port changed", func(l *networkingv1.NetworkPolicy) {
			l.Spec.Ingress[0].Ports = appendPolicyPort(nil, corev1.ProtocolTCP, 9090)
		}, false},
		{"deleting", func(l *networkingv1.NetworkPolicy) { l.DeletionTimestamp = &now }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := desired()
			tc.mutate(live)
			if got := policyUpToDate(live, desired()); got != tc.want {
				t.Fatalf("policyUpToDate = %v, want %v", got, tc.want)
			}
		})
	}
}

// One namespace failing to apply must not starve the namespaces after it:
// they still get their policies, the stale sweep still runs (sparing the
// failed namespace), and the reconcile reports the failure and retries.
func TestReconcile_ApplyFailureDoesNotStarveLaterNamespaces(t *testing.T) {
	stamped := func(ns string) *corev1.Pod {
		p := testutil.Pod(ns, "p", map[string]string{SystemLabelKey("a", "v"): "both"})
		p.Annotations = map[string]string{AnnotationResolvedGeneration: "1"}
		return p
	}
	stale := func(ns, name string) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Labels: map[string]string{LabelManagedBy: LabelManagedByValue, LabelNetwork: "a.v"},
		}}
	}
	vnet := testutil.VirtualNetwork("v", "a", &vnetv1alpha1.NamespaceSelector{Names: []string{"b", "c"}})
	c, _ := patchCountingClient(t, "b",
		mkNamespace("a", nil), mkNamespace("b", nil), mkNamespace("c", nil), mkNamespace("d", nil),
		vnet, stamped("a"), stamped("b"), stamped("c"),
		stale("b", "legacy-name"), // spared: its replacement failed to apply
		stale("d", "legacy-name"), // swept: d has no members
	)
	rec := &fakeRecorder{}
	r := &VirtualNetworkReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vnet)})
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reconcile err = %v, want the apply error so the failed namespace is retried", err)
	}

	for _, ns := range []string{"a", "c"} {
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: PolicyName("v", "a")}, &networkingv1.NetworkPolicy{}); err != nil {
			t.Errorf("namespace %s has no membership policy: %v", ns, err)
		}
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "b", Name: "legacy-name"}, &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("stale policy in the failed namespace was swept: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "d", Name: "legacy-name"}, &networkingv1.NetworkPolicy{}); err == nil {
		t.Error("stale policy in a memberless namespace survived the sweep")
	}

	got := &vnetv1alpha1.VirtualNetwork{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(vnet), got); err != nil {
		t.Fatal(err)
	}
	ready := conditionFor(got, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonApplyFailed {
		t.Fatalf("Ready = %+v, want False/%s", ready, ReasonApplyFailed)
	}
	if len(got.Status.GeneratedPolicies) != 2 {
		t.Errorf("status.generatedPolicies = %v, want the two applied", got.Status.GeneratedPolicies)
	}
	failed := 0
	for _, reason := range rec.reasons {
		if reason == EventApplyFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("ApplyFailed events = %d, want 1 (reasons %v)", failed, rec.reasons)
	}
}

// conditionFor returns vnet's condition of type t, or nil.
func conditionFor(vnet *vnetv1alpha1.VirtualNetwork, t string) *metav1.Condition {
	for i := range vnet.Status.Conditions {
		if vnet.Status.Conditions[i].Type == t {
			return &vnet.Status.Conditions[i]
		}
	}
	return nil
}
