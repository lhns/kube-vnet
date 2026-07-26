package controller

import (
	"context"
	"slices"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// The ResolutionReconciler watches VirtualNetwork because a rule *references* a
// vnet, making vnet existence an input to resolution. Without it, a rule naming
// a not-yet-created vnet resolves to "no stamp" and is never revisited — the
// pod predicate is change-based, so the informer resync (old == new) is
// filtered and the pod stays isolated until edited or recreated. See ADR 0030
// (amended). These tests cover which pods a vnet event fans out to.

func fanoutReconciler(objs ...k8sruntime.Object) *ResolutionReconciler {
	scheme := k8sruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = vnetv1alpha1.AddToScheme(scheme)
	return &ResolutionReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		Scheme:   scheme,
		NSFilter: NewNamespaceFilter(nil),
	}
}

func fanoutPod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

func fanoutBinding(ns, name string, r vnetv1alpha1.VirtualNetworkRef) *vnetv1alpha1.VirtualNetworkBinding {
	return &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       vnetv1alpha1.VirtualNetworkBindingSpec{VirtualNetworkRef: r},
	}
}

// gotPods returns the fanned-out pod names as "ns/name", sorted.
func gotPods(t *testing.T, r *ResolutionReconciler, vnet *vnetv1alpha1.VirtualNetwork) []string {
	t.Helper()
	reqs := r.vnetToAffectedPods(context.Background(), vnet)
	out := make([]string, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, req.Namespace+"/"+req.Name)
	}
	sort.Strings(out)
	return out
}

// Join-label sources. The bare form is only honored in the vnet's home
// namespace (ADR 0022), so a bare label elsewhere must NOT fan out — it names
// a different (or no) vnet.
func TestVnetToAffectedPods_JoinLabels(t *testing.T) {
	vnet := mkVnet("payments", "platform", nil)
	r := fanoutReconciler(
		fanoutPod("platform", "bare-home", map[string]string{"kube-vnet/net.payments": "both"}),
		fanoutPod("webapp", "bare-foreign", map[string]string{"kube-vnet/net.payments": "both"}),
		fanoutPod("webapp", "prefixed", map[string]string{"kube-vnet/net.platform.payments": "both"}),
		fanoutPod("webapp", "other-vnet", map[string]string{"kube-vnet/net.platform.orders": "both"}),
		fanoutPod("webapp", "unlabelled", map[string]string{"app": "web"}),
	)

	want := []string{"platform/bare-home", "webapp/prefixed"}
	if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (bare form outside the home NS must not fan out)", got, want)
	}
}

// A binding is a first-class membership source, so a binding applied before its
// vnet hits the same stuck-forever shape as a join label.
func TestVnetToAffectedPods_Bindings(t *testing.T) {
	vnet := mkVnet("payments", "platform", nil)

	t.Run("explicit matching namespace fans out", func(t *testing.T) {
		r := fanoutReconciler(
			fanoutBinding("webapp", "b", ref("payments", "platform")),
			fanoutPod("webapp", "p", nil),
			fanoutPod("elsewhere", "q", nil),
		)
		want := []string{"webapp/p"}
		if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("binding naming another vnet does not fan out", func(t *testing.T) {
		r := fanoutReconciler(
			fanoutBinding("webapp", "b", ref("orders", "platform")),
			fanoutPod("webapp", "p", nil),
		)
		if got := gotPods(t, r, vnet); len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	// An omitted namespace means "the vnet of this name in the binding's own
	// namespace", so it resolves to this vnet only from the vnet's namespace.
	t.Run("omitted namespace only matches from the vnet's own namespace", func(t *testing.T) {
		r := fanoutReconciler(
			fanoutBinding("platform", "local", ref("payments", "")),
			fanoutBinding("webapp", "foreign", ref("payments", "")),
			fanoutPod("platform", "p", nil),
			fanoutPod("webapp", "q", nil),
		)
		want := []string{"platform/p"}
		if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v (webapp's omitted ref means webapp.payments, a different vnet)", got, want)
		}
	})
}

func TestVnetToAffectedPods_Baselines(t *testing.T) {
	vnet := mkVnet("payments", "platform", nil)
	membership := func(r vnetv1alpha1.VirtualNetworkRef) vnetv1alpha1.BaselineMembership {
		return vnetv1alpha1.BaselineMembership{VirtualNetworkRef: r, Direction: "both"}
	}

	t.Run("namespace baseline fans out its own namespace", func(t *testing.T) {
		r := fanoutReconciler(
			&vnetv1alpha1.VirtualNetworkBaseline{
				ObjectMeta: metav1.ObjectMeta{Namespace: "webapp", Name: "default"},
				Spec: vnetv1alpha1.VirtualNetworkBaselineSpec{
					Memberships: []vnetv1alpha1.BaselineMembership{membership(ref("payments", "platform"))},
				},
			},
			fanoutPod("webapp", "p", nil),
			fanoutPod("elsewhere", "q", nil),
		)
		want := []string{"webapp/p"}
		if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("cluster baseline with explicit ref fans out cluster-wide", func(t *testing.T) {
		r := fanoutReconciler(
			&vnetv1alpha1.ClusterVirtualNetworkBaseline{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{
					Memberships: []vnetv1alpha1.BaselineMembership{membership(ref("payments", "platform"))},
				},
			},
			fanoutPod("webapp", "p", nil),
			fanoutPod("elsewhere", "q", nil),
		)
		want := []string{"elsewhere/q", "webapp/p"}
		if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	// The important scoping case: an omitted namespace in the CLUSTER baseline
	// means a different vnet per pod namespace, so it must not fan out
	// cluster-wide for this one vnet.
	t.Run("cluster baseline with omitted ref scopes to the vnet's namespace", func(t *testing.T) {
		r := fanoutReconciler(
			&vnetv1alpha1.ClusterVirtualNetworkBaseline{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{
					Memberships: []vnetv1alpha1.BaselineMembership{membership(ref("payments", ""))},
				},
			},
			fanoutPod("platform", "p", nil),
			fanoutPod("webapp", "q", nil),
		)
		want := []string{"platform/p"}
		if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v (webapp's omitted ref means webapp.payments)", got, want)
		}
	})
}

// A pod reachable through several sources must be enqueued once.
func TestVnetToAffectedPods_Dedupes(t *testing.T) {
	vnet := mkVnet("payments", "platform", nil)
	r := fanoutReconciler(
		fanoutPod("webapp", "p", map[string]string{"kube-vnet/net.platform.payments": "both"}),
		fanoutBinding("webapp", "b", ref("payments", "platform")),
		&vnetv1alpha1.VirtualNetworkBaseline{
			ObjectMeta: metav1.ObjectMeta{Namespace: "webapp", Name: "default"},
			Spec: vnetv1alpha1.VirtualNetworkBaselineSpec{
				Memberships: []vnetv1alpha1.BaselineMembership{
					{VirtualNetworkRef: ref("payments", "platform"), Direction: "both"},
				},
			},
		},
	)

	want := []string{"webapp/p"}
	if got := gotPods(t, r, vnet); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (label + binding + baseline must dedupe to one request)", got, want)
	}
}

// Churn guard for the watch predicate. VirtualNetwork has a status subresource,
// so generation bumps only on spec changes: the vnet controller's per-membership
// status writes must NOT fan out to pods, or the reconcile loop removed in
// 75c14a6 comes back. Create/Delete stay at the predicate's default true so a
// vnet appearing still re-resolves.
func TestVnetWatchPredicate_IgnoresStatusWrites(t *testing.T) {
	p := predicate.GenerationChangedPredicate{}

	base := mkVnet("payments", "platform", nil)
	base.Generation = 1

	if !p.Create(event.CreateEvent{Object: base}) {
		t.Error("Create must fire — a vnet appearing is the whole point of the watch")
	}
	if !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("Delete must fire so members are re-resolved when the vnet goes away")
	}

	statusOnly := base.DeepCopy()
	statusOnly.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled",
		LastTransitionTime: metav1.Now(),
	}}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusOnly}) {
		t.Fatal("status-only update fired; every membership status write would fan out to pods")
	}

	specChanged := base.DeepCopy()
	specChanged.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: specChanged}) {
		t.Error("spec change must fire — allowedNamespaces edits change who may join")
	}
}
