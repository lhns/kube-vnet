package controller

import (
	"context"
	"errors"
	"reflect"
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
// resolution input paths. A transient apiserver error must propagate (the
// caller requeues with backoff) rather than collapse to "no rules" or "not
// permitted", which would strip valid stamps during an apiserver blip.

var errInjected = errors.New("injected transient apiserver error")

var resolutionSchemeForTest = testutil.Scheme

func testPod(ns string) *corev1.Pod { return testutil.Pod(ns, "p", nil) }

func TestResolverInputs_TransientErrorPropagates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		fails any // the type of the object or list whose read fails
		call  func(*Resolver) error
	}{
		{"cluster_baseline", &vnetv1alpha1.ClusterVirtualNetworkBaseline{}, func(r *Resolver) error {
			_, err := r.clusterBaselineRules(ctx, testPod("ns1"))
			return err
		}},
		{"namespace_baseline", &vnetv1alpha1.VirtualNetworkBaseline{}, func(r *Resolver) error {
			_, err := r.namespaceBaselineRules(ctx, testPod("ns1"))
			return err
		}},
		{"bindings", &vnetv1alpha1.VirtualNetworkBindingList{}, func(r *Resolver) error {
			_, err := r.bindingRules(ctx, testPod("ns1"))
			return err
		}},
		// Permits Gets the vnet: the rule must not be silently dropped.
		{"permits", &vnetv1alpha1.VirtualNetwork{}, func(r *Resolver) error {
			_, err := r.filterPermittedRules(ctx, []ResolutionRule{{Vnet: "other.v", Direction: DirectionBoth, Source: "test"}}, "ns1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failing := reflect.TypeOf(tc.fails)
			c := fake.NewClientBuilder().WithScheme(resolutionSchemeForTest(t)).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if reflect.TypeOf(obj) == failing {
							return errInjected
						}
						return cl.Get(ctx, key, obj, opts...)
					},
					List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if reflect.TypeOf(list) == failing {
							return errInjected
						}
						return cl.List(ctx, list, opts...)
					},
				}).Build()
			if err := tc.call(&Resolver{Reader: c}); !errors.Is(err, errInjected) {
				t.Errorf("transient error should propagate, got err=%v", err)
			}
		})
	}
}

func TestClusterBaselineRules_NotFoundIsNotAnError(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no baseline exists

	r := &Resolver{Reader: c}
	rules, err := r.clusterBaselineRules(context.Background(), testPod("ns1"))
	if err != nil {
		t.Errorf("NotFound should mean 'no baseline', not an error: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected no rules, got %v", rules)
	}
}

func TestFilterPermittedRules_NotPermittedStillDropsSilently(t *testing.T) {
	scheme := resolutionSchemeForTest(t)
	// Vnet genuinely doesn't exist → NotFound inside Permits → rule
	// dropped, NO error. The legitimate deny path is unchanged.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &Resolver{Reader: c}
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
