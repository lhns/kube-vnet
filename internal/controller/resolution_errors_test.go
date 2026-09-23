package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/testutil"
)

// These tests pin the NotFound-vs-transient error contract of the
// resolution input paths. A transient apiserver error MUST propagate
// (caller requeues with backoff) rather than silently collapse to "no
// rules" / "not permitted" — that collapse stripped valid stamps from
// pods during apiserver blips, causing momentary membership loss with
// no requeue to recover.

var errInjected = errors.New("injected transient apiserver error")

var resolutionSchemeForTest = testutil.Scheme

func testPod(ns string) *corev1.Pod { return testutil.Pod(ns, "p", nil) }

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

	r := &Resolver{Reader: c, NSFilter: NewNamespaceFilter(nil)}
	_, err := r.clusterBaselineRules(context.Background(), testPod("ns1"))
	if !errors.Is(err, errInjected) {
		t.Errorf("transient Get error should propagate, got err=%v", err)
	}
}

func TestClusterBaselineRules_NotFoundIsNotAnError(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no baseline exists

	r := &Resolver{Reader: c, NSFilter: NewNamespaceFilter(nil)}
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

	r := &Resolver{Reader: c, NSFilter: NewNamespaceFilter(nil)}
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

	r := &Resolver{Reader: c, NSFilter: NewNamespaceFilter(nil)}
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

	r := &Resolver{Reader: c, NSFilter: NewNamespaceFilter(nil)}
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

	r := &Resolver{Reader: c, NSFilter: NewNamespaceFilter(nil)}
	rules := []ResolutionRule{{Vnet: VnetKey("other.ghost"), Direction: DirectionBoth, Source: "test"}}
	out, err := r.filterPermittedRules(context.Background(), rules, "ns1")
	if err != nil {
		t.Errorf("vnet-not-found is a deny, not an error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("rule for missing vnet should be dropped, got %v", out)
	}
}

// Disabling a namespace removes every trace of resolution from its pods,
// including the resolved-by marker, not just the stamps.
func TestResolutionReconciler_DisabledNamespace_StripsAllMarkers(t *testing.T) {
	ns := mkNamespace("off", nil)
	ns.Annotations = map[string]string{AnnotationDisabled: "true"}
	pod := testPod("off")
	pod.Labels = map[string]string{LabelSystemNetPrefix + "off.v": "both", "app": "web"}
	pod.Annotations = map[string]string{
		AnnotationResolvedGeneration: "1",
		AnnotationResolvedBy:         ResolvedByAdmission,
	}
	c := fake.NewClientBuilder().WithScheme(resolutionSchemeForTest(t)).WithObjects(ns, pod).Build()
	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil)}

	key := client.ObjectKeyFromObject(pod)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels["app"] != "web" {
		t.Errorf("labels = %v, want only app=web", got.Labels)
	}
	for _, a := range []string{AnnotationResolvedGeneration, AnnotationResolvedBy} {
		if v, ok := got.Annotations[a]; ok {
			t.Errorf("annotation %s=%q survived", a, v)
		}
	}
}
