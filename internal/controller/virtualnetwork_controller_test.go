package controller

import (
	"context"
	"errors"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/testutil"
)

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
