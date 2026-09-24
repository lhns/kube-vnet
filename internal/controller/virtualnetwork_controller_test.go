package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/testutil"
)

// discoverMembers trusts only resolved system stamps from namespaces that are
// managed and permitted now, and reports user join labels it cannot honor.
func TestDiscoverMembers_EligibilityAndDiagnostics(t *testing.T) {
	stamped := func(ns, name, dir string, userLabels map[string]string) *corev1.Pod {
		p := testutil.Pod(ns, name, userLabels)
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		p.Labels[SystemLabelKey("home", "v")] = dir
		p.Annotations = map[string]string{AnnotationResolvedGeneration: "1"}
		return p
	}
	prefixed := func(dir string) map[string]string {
		return map[string]string{DefaultLabelPrefix + "net.home.v": dir}
	}
	unresolved := stamped("home", "unresolved", "both", nil)
	unresolved.Annotations = nil

	r := newReconciler(
		mkNamespace("home", nil), mkNamespace("allowed", nil), mkNamespace("foreign", nil),
		testutil.Namespace("off", map[string]string{AnnotationDisabled: "true"}, nil),
		mkVnet("v", "home", &vnetv1alpha1.NamespaceSelector{Names: []string{"allowed", "off"}}),
		stamped("home", "member", "both", nil),
		stamped("home", "opted-out", "none", nil),
		stamped("allowed", "egress-member", "egress", prefixed("egress")),
		unresolved,
		// Stale stamps: the namespace is no longer eligible.
		stamped("foreign", "stale", "both", prefixed("both")),
		stamped("off", "stale", "both", prefixed("both")),
		// A bad user label is reported but does not revoke a valid stamp.
		stamped("allowed", "typo", "ingress", prefixed("bogus")),
		// A namespace the vnet doesn't admit is not reported, whatever its
		// label says: any tenant could otherwise degrade the vnet.
		testutil.Pod("foreign", "prober", prefixed("bogus")),
	)
	vnet := &vnetv1alpha1.VirtualNetwork{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "home", Name: "v"}, vnet); err != nil {
		t.Fatal(err)
	}

	members, invalid, err := r.discoverMembers(context.Background(), vnet)
	if err != nil {
		t.Fatalf("discoverMembers: %v", err)
	}
	wantMembers := map[string]map[Direction][]string{
		"home":    {DirectionBoth: {"member"}},
		"allowed": {DirectionEgress: {"egress-member"}, DirectionIngress: {"typo"}},
	}
	if !reflect.DeepEqual(members, wantMembers) {
		t.Errorf("members = %v, want %v", members, wantMembers)
	}
	gotInvalid := map[string]string{}
	for _, j := range invalid {
		gotInvalid[j.PodNamespace+"/"+j.PodName] = j.Reason
	}
	wantInvalid := map[string]string{
		"off/stale":    ReasonNamespaceExcluded,
		"allowed/typo": ReasonInvalidDirection,
	}
	if !reflect.DeepEqual(gotInvalid, wantInvalid) {
		t.Errorf("invalid = %v, want %v", gotInvalid, wantInvalid)
	}
}

// Disabling a vnet's home namespace drops its policies and status members, so
// the members gauge must drop to zero too instead of keeping its last count.
func TestReconcile_HomeNamespaceExcluded_ZeroesMembersGauge(t *testing.T) {
	home := testutil.Namespace("gauge-home", map[string]string{AnnotationDisabled: "true"}, nil)
	vnet := mkVnet("v", "gauge-home", nil)
	c := fake.NewClientBuilder().
		WithScheme(testutil.Scheme(t, networkingv1.AddToScheme)).
		WithObjects(home, vnet).
		WithStatusSubresource(&vnetv1alpha1.VirtualNetwork{}).
		Build()
	r := &VirtualNetworkReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	setMembers("gauge-home", "v", 2) // left by an earlier, managed reconcile
	t.Cleanup(func() { clearMembers("gauge-home", "v") })

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vnet)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := promtestutil.ToFloat64(membersByNetwork.WithLabelValues("gauge-home/v")); got != 0 {
		t.Errorf("members gauge = %v, want 0", got)
	}
}

// A vnet whose home namespace is disabled must drop its membership policies.
// If that delete fails, the reconcile must fail too: returning success leaves
// the stale grants in place with no retry.
func TestReconcile_HomeNamespaceExcluded_DeleteErrorRequeues(t *testing.T) {
	home := testutil.Namespace("home", map[string]string{AnnotationDisabled: "true"}, nil)
	vnet := mkVnet("v", "home", nil)
	stale := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Namespace: "member", Name: PolicyName("v", "home"),
		Labels: map[string]string{LabelManagedBy: LabelManagedByValue, LabelNetwork: "home.v"},
	}}
	c := fake.NewClientBuilder().
		WithScheme(testutil.Scheme(t, networkingv1.AddToScheme)).
		WithObjects(home, vnet, stale).
		WithStatusSubresource(&vnetv1alpha1.VirtualNetwork{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return errInjected
			},
		}).Build()
	r := &VirtualNetworkReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vnet)})
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reconcile err = %v, want the delete error so the stale policy is retried", err)
	}
}
