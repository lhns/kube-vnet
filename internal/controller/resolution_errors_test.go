package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// These tests pin the NotFound-vs-transient error contract of the
// resolution input paths. A transient apiserver error MUST propagate
// (caller requeues with backoff) rather than silently collapse to "no
// rules" / "not permitted" — that collapse stripped valid stamps from
// pods during apiserver blips, causing momentary membership loss with
// no requeue to recover.

var errInjected = errors.New("injected transient apiserver error")

func resolutionSchemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := vnetv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("vnetv1alpha1.AddToScheme: %v", err)
	}
	return s
}

func testPod(ns string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"}}
}

func TestClusterBaselineRules_TransientErrorPropagates(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isCB := obj.(*vnetv1alpha1.ClusterVirtualNetworkBaseline); isCB {
					return errInjected
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	_, err := r.clusterBaselineRules(context.Background(), testPod("ns1"))
	if !errors.Is(err, errInjected) {
		t.Errorf("transient Get error should propagate, got err=%v", err)
	}
}

func TestClusterBaselineRules_NotFoundIsNotAnError(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no baseline exists

	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	rules, err := r.clusterBaselineRules(context.Background(), testPod("ns1"))
	if err != nil {
		t.Errorf("NotFound should mean 'no baseline', not an error: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected no rules, got %v", rules)
	}
}

func TestNamespaceBaselineRules_TransientErrorPropagates(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isNB := obj.(*vnetv1alpha1.VirtualNetworkBaseline); isNB {
					return errInjected
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	_, err := r.namespaceBaselineRules(context.Background(), testPod("ns1"))
	if !errors.Is(err, errInjected) {
		t.Errorf("transient Get error should propagate, got err=%v", err)
	}
}

func TestBindingRules_TransientListErrorPropagates(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isVNBList := list.(*vnetv1alpha1.VirtualNetworkBindingList); isVNBList {
					return errInjected
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	_, err := r.bindingRules(context.Background(), testPod("ns1"))
	if !errors.Is(err, errInjected) {
		t.Errorf("transient List error should propagate, got err=%v", err)
	}
}

func TestFilterPermittedRules_TransientErrorPropagates(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	// Permits() Gets the VirtualNetwork; inject an error there.
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isVnet := obj.(*vnetv1alpha1.VirtualNetwork); isVnet {
					return errInjected
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	rules := []ResolutionRule{{Vnet: VnetKey("other.v"), Direction: DirectionBoth, Source: "test"}}
	_, err := r.filterPermittedRules(context.Background(), rules, "ns1")
	if !errors.Is(err, errInjected) {
		t.Errorf("transient Permits error should propagate (not silently drop the rule), got err=%v", err)
	}
}

func TestFilterPermittedRules_NotPermittedStillDropsSilently(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	// Vnet genuinely doesn't exist → NotFound inside Permits → rule
	// dropped, NO error. The legitimate deny path is unchanged.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}
	rules := []ResolutionRule{{Vnet: VnetKey("other.ghost"), Direction: DirectionBoth, Source: "test"}}
	out, err := r.filterPermittedRules(context.Background(), rules, "ns1")
	if err != nil {
		t.Errorf("vnet-not-found is a deny, not an error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("rule for missing vnet should be dropped, got %v", out)
	}
}
