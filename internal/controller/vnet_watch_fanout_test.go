package controller

import (
	"context"
	"slices"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Fan-out is expressed as "which namespaces does this vnet admit" rather than
// by matching each membership source (ADR 0044). A pod outside the admitted set
// cannot become a member however it names the vnet, so re-resolving it could
// not change anything — which makes the single question cover join labels,
// bindings and both baselines at once.
//
// This REPLACES an earlier per-source table. Cases like "a binding naming a
// different vnet does not fan out" are deliberately gone: such a pod now IS
// enqueued when its namespace is admitted. That is a superset, and an
// intentional trade — a redundant resolution reconcile is idempotent and cheap,
// whereas per-source matching duplicated permission logic four times and had to
// special-case omitted-namespace refs.

func fanoutScheme() *k8sruntime.Scheme {
	scheme := k8sruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = vnetv1alpha1.AddToScheme(scheme)
	return scheme
}

func fanoutClient(objs ...k8sruntime.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(fanoutScheme()).WithRuntimeObjects(objs...).Build()
}

func fanoutReconciler(objs ...k8sruntime.Object) *ResolutionReconciler {
	scheme := fanoutScheme()
	return &ResolutionReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		Scheme:   scheme,
		NSFilter: NewNamespaceFilter(nil),
	}
}

func fanoutPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func sortedNames(reqs []string) []string { sort.Strings(reqs); return reqs }

// NamespacesAdmittedBy is the single definition of a vnet's blast radius, so it
// carries the weight of the design. It must agree with Permits in every case.
func TestNamespacesAdmittedBy(t *testing.T) {
	nss := []k8sruntime.Object{
		mkNamespace("platform", nil),
		mkNamespace("webapp", map[string]string{"tier": "prod"}),
		mkNamespace("staging", map[string]string{"tier": "dev"}),
	}

	for _, tc := range []struct {
		name    string
		allowed *vnetv1alpha1.NamespaceSelector
		want    []string
	}{
		{
			// The default, and the common case: a vnet admits only its home.
			name:    "nil selector is home-only",
			allowed: nil,
			want:    []string{"platform"},
		},
		{
			name:    "names, plus home implicitly",
			allowed: &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp"}},
			want:    []string{"platform", "webapp"},
		},
		{
			name: "selector matches by namespace label",
			allowed: &vnetv1alpha1.NamespaceSelector{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
			want: []string{"platform", "webapp"},
		},
		{
			name:    "all admits every namespace",
			allowed: &vnetv1alpha1.NamespaceSelector{All: true},
			want:    []string{"platform", "staging", "webapp"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fanoutClient(append(append([]k8sruntime.Object{}, nss...),
				mkVnet("payments", "platform", tc.allowed))...)
			got, err := NamespacesAdmittedBy(context.Background(), c,
				mkVnet("payments", "platform", tc.allowed))
			if err != nil {
				t.Fatalf("NamespacesAdmittedBy: %v", err)
			}
			if !slices.Equal(sortedNames(got), tc.want) {
				t.Fatalf("got %v, want %v", sortedNames(got), tc.want)
			}
		})
	}
}

// The per-namespace `namespace` system vnet is by far the most common vnet
// (one per managed namespace), so it must stay tight — if it fanned out
// cluster-wide the watch would be a churn source rather than a fix.
func TestNamespacesAdmittedBy_SystemNamespaceVnetStaysTight(t *testing.T) {
	c := fanoutClient(
		mkNamespace("a", nil), mkNamespace("b", nil), mkNamespace("c", nil),
		mkVnet(SystemVnetNamespace, "a", nil),
	)
	got, err := NamespacesAdmittedBy(context.Background(), c, mkVnet(SystemVnetNamespace, "a", nil))
	if err != nil {
		t.Fatalf("NamespacesAdmittedBy: %v", err)
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %v, want [a] — the common vnet must not fan out cluster-wide", got)
	}
}

// The mapper is then just "pods in those namespaces", whatever named the vnet.
func TestVnetToAffectedPods_IsPodsInAdmittedNamespaces(t *testing.T) {
	vnet := mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp"}})
	r := fanoutReconciler(
		mkNamespace("platform", nil), mkNamespace("webapp", nil), mkNamespace("elsewhere", nil),
		vnet,
		fanoutPod("platform", "home"),
		fanoutPod("webapp", "permitted"),
		fanoutPod("elsewhere", "unrelated"), // not admitted -> cannot change state
	)

	reqs := r.vnetToAffectedPods(context.Background(), vnet)
	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.Namespace+"/"+req.Name)
	}
	want := []string{"platform/home", "webapp/permitted"}
	if !slices.Equal(sortedNames(got), want) {
		t.Fatalf("got %v, want %v", sortedNames(got), want)
	}
}

// podsIn deduplicates, so overlapping namespace sets can't enqueue a pod twice.
func TestPodsIn_Dedupes(t *testing.T) {
	r := fanoutReconciler(mkNamespace("a", nil), fanoutPod("a", "p"))
	if got := r.podsIn(context.Background(), "a", "a", ""); len(got) != 1 {
		t.Fatalf("got %d requests, want 1 (deduped)", len(got))
	}
}

// nsToVnets is the same question inverted: which vnets does a namespace event
// affect? Both the vnets living in it and the vnets admitting it.
func TestNsToVnets(t *testing.T) {
	r := &VirtualNetworkReconciler{
		Client: fanoutClient(
			mkNamespace("platform", nil), mkNamespace("webapp", nil), mkNamespace("other", nil),
			mkVnet("home-here", "webapp", nil),
			mkVnet("admits-it", "platform", &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp"}}),
			mkVnet("unrelated", "platform", &vnetv1alpha1.NamespaceSelector{Names: []string{"other"}}),
		),
		NSFilter: NewNamespaceFilter(nil),
	}

	reqs := r.nsToVnets(context.Background(), mkNamespace("webapp", nil))
	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.Namespace+"/"+req.Name)
	}
	want := []string{"platform/admits-it", "webapp/home-here"}
	if !slices.Equal(sortedNames(got), want) {
		t.Fatalf("got %v, want %v", sortedNames(got), want)
	}
}

// A binding only ever selects pods in its own namespace, which is what makes
// both of its new watches collapse into one trivial mapper.
func TestBindingsInNamespaceOf(t *testing.T) {
	scheme := fanoutScheme()
	r := &VirtualNetworkBindingReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
			&vnetv1alpha1.VirtualNetworkBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "webapp", Name: "b1"}},
			&vnetv1alpha1.VirtualNetworkBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "webapp", Name: "b2"}},
			&vnetv1alpha1.VirtualNetworkBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "b3"}},
		).Build(),
		Scheme:   scheme,
		NSFilter: NewNamespaceFilter(nil),
	}

	t.Run("from a pod", func(t *testing.T) {
		reqs := r.bindingsInNamespaceOf(context.Background(), fanoutPod("webapp", "p"))
		if len(reqs) != 2 {
			t.Fatalf("got %d bindings, want 2", len(reqs))
		}
	})
	t.Run("from the namespace itself", func(t *testing.T) {
		reqs := r.bindingsInNamespaceOf(context.Background(), mkNamespace("webapp", nil))
		if len(reqs) != 2 {
			t.Fatalf("got %d bindings, want 2 (cluster-scoped: name is the namespace)", len(reqs))
		}
	})
}

// Churn guard. VirtualNetwork has a status subresource, so generation bumps
// only on spec changes: the vnet controller's per-membership status writes must
// NOT fan out to pods, or the reconcile loop removed in 75c14a6 comes back.
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
