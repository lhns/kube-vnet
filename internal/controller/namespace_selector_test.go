package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func newReconciler(objs ...runtime.Object) *VirtualNetworkReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = vnetv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &VirtualNetworkReconciler{
		Client:   c,
		Scheme:   scheme,
		NSFilter: NewNamespaceFilter(nil),
	}
}

func TestPermits_HomeNamespaceAlwaysAllowed(t *testing.T) {
	r := newReconciler()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
	}
	ok, err := r.permits(context.Background(), vnet, "home")
	if err != nil || !ok {
		t.Fatalf("home should be permitted; ok=%v err=%v", ok, err)
	}
}

func TestPermits_NoSelectorRejectsForeign(t *testing.T) {
	r := newReconciler()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
	}
	ok, _ := r.permits(context.Background(), vnet, "other")
	if ok {
		t.Errorf("foreign ns must be rejected when no allowedNamespaces is set")
	}
}

func TestPermits_All(t *testing.T) {
	r := newReconciler()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{All: true},
		},
	}
	ok, _ := r.permits(context.Background(), vnet, "anything")
	if !ok {
		t.Errorf("All should permit any namespace")
	}
}

func TestPermits_Names(t *testing.T) {
	r := newReconciler()
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp", "monitoring"}},
		},
	}
	for _, ns := range []string{"webapp", "monitoring"} {
		ok, _ := r.permits(context.Background(), vnet, ns)
		if !ok {
			t.Errorf("%s should be permitted", ns)
		}
	}
	ok, _ := r.permits(context.Background(), vnet, "other")
	if ok {
		t.Errorf("other should be rejected")
	}
}

func TestPermits_Selector(t *testing.T) {
	prod := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-app", Labels: map[string]string{"tier": "prod"}},
	}
	dev := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-app", Labels: map[string]string{"tier": "dev"}},
	}
	r := newReconciler(prod, dev)
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		},
	}
	ok, _ := r.permits(context.Background(), vnet, "prod-app")
	if !ok {
		t.Errorf("prod-app should match")
	}
	ok, _ = r.permits(context.Background(), vnet, "dev-app")
	if ok {
		t.Errorf("dev-app should not match")
	}
}

func TestPermits_NamesAndSelectorUnion(t *testing.T) {
	labeled := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "labeled", Labels: map[string]string{"join": "yes"}},
	}
	r := newReconciler(labeled)
	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "home"},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{
				Names:    []string{"explicit"},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"join": "yes"}},
			},
		},
	}
	if ok, _ := r.permits(context.Background(), vnet, "explicit"); !ok {
		t.Errorf("explicit name should match")
	}
	if ok, _ := r.permits(context.Background(), vnet, "labeled"); !ok {
		t.Errorf("labeled namespace should match")
	}
	if ok, _ := r.permits(context.Background(), vnet, "neither"); ok {
		t.Errorf("neither should not match")
	}
}

// A bare `kube-vnet/net.namespace` label names the pod's own namespace vnet.
// Only `cluster` is reachable by its bare name from every namespace, so a
// malformed label in one namespace must not be reported on the `namespace`
// vnet of every other namespace.
func TestDiscoverMembers_BareNamespaceLabel_OnlyDiagnosedAtHome(t *testing.T) {
	pod := func(ns string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "p",
			Labels: map[string]string{DefaultLabelPrefix + "net." + SystemVnetNamespace: "bogus"},
		}}
	}
	r := newReconciler(mkNamespace("a", nil), mkNamespace("b", nil), pod("a"), pod("b"))

	for _, vnet := range []*vnetv1alpha1.VirtualNetwork{
		mkVnet(SystemVnetNamespace, "a", nil),
		mkVnet(SystemVnetNamespace, "b", nil),
	} {
		_, invalid, err := r.discoverMembers(context.Background(), vnet)
		if err != nil {
			t.Fatalf("discoverMembers: %v", err)
		}
		if len(invalid) != 1 || invalid[0].PodNamespace != vnet.Namespace {
			t.Errorf("%s/%s: invalid = %+v, want only the pod in %q", vnet.Namespace, vnet.Name, invalid, vnet.Namespace)
		}
	}
}
