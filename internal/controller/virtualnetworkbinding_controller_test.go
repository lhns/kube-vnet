package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/testutil"
)

// reconcileBinding runs one binding reconcile over objs and returns the
// persisted binding.
func reconcileBinding(t *testing.T, filter *NamespaceFilter, b *vnetv1alpha1.VirtualNetworkBinding, objs ...client.Object) *vnetv1alpha1.VirtualNetworkBinding {
	t.Helper()
	ctx := context.Background()
	c := fake.NewClientBuilder().
		WithScheme(schemeForPermits(t)).
		WithObjects(append(objs, b)...).
		WithStatusSubresource(&vnetv1alpha1.VirtualNetworkBinding{}).
		Build()
	r := &VirtualNetworkBindingReconciler{Client: c, NSFilter: filter}
	key := client.ObjectKeyFromObject(b)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &vnetv1alpha1.VirtualNetworkBinding{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	return got
}

func webBinding(vnetName, vnetNS string) *vnetv1alpha1.VirtualNetworkBinding {
	return &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "webapp", Name: "b"},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: ref(vnetName, vnetNS),
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
}

// selectedPod is a pod the web binding selects. stamp is its membership stamp
// for webapp/payments ("" for none); resolved sets the resolved marker.
func selectedPod(name, stamp string, resolved bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "webapp", Name: name, Labels: map[string]string{"app": "web"},
	}}
	if stamp != "" {
		p.Labels[SystemLabelKey("webapp", "payments")] = stamp
	}
	if resolved {
		p.Annotations = map[string]string{AnnotationResolvedGeneration: "0"}
	}
	return p
}

func readyCondition(t *testing.T, b *vnetv1alpha1.VirtualNetworkBinding) metav1.Condition {
	t.Helper()
	for _, c := range b.Status.Conditions {
		if c.Type == "Ready" {
			return c
		}
	}
	t.Fatalf("no Ready condition in %+v", b.Status.Conditions)
	return metav1.Condition{}
}

// attachedPods lists only the selected pods resolution made members: a pod
// can match the selector and still not be a member (a baseline or pod-label
// conflict resolved to none, the stamp not yet written, ...).
func TestBindingStatus_AttachedPodsAreMembersOnly(t *testing.T) {
	objs := []client.Object{
		mkNamespace("webapp", nil), mkVnet("payments", "webapp", nil),
		selectedPod("member", "ingress", true),
		selectedPod("none", "none", true),
		selectedPod("unresolved", "both", false),
		selectedPod("unstamped", "", true),
		// Stamped for another vnet only.
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "webapp", Name: "elsewhere",
			Labels:      map[string]string{"app": "web", SystemLabelKey("webapp", "other"): "both"},
			Annotations: map[string]string{AnnotationResolvedGeneration: "0"},
		}},
		// A member not selected by this binding.
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "webapp", Name: "unselected",
			Labels:      map[string]string{"app": "db", SystemLabelKey("webapp", "payments"): "both"},
			Annotations: map[string]string{AnnotationResolvedGeneration: "0"},
		}},
	}
	got := reconcileBinding(t, NewNamespaceFilter(nil), webBinding("payments", ""), objs...)

	if want := []string{"member"}; !slices.Equal(got.Status.AttachedPods, want) {
		t.Errorf("attachedPods = %v, want %v", got.Status.AttachedPods, want)
	}
	c := readyCondition(t, got)
	if c.Status != metav1.ConditionTrue || c.Reason != ReasonBindingPodsAttached {
		t.Errorf("Ready = %s/%s, want True/%s", c.Status, c.Reason, ReasonBindingPodsAttached)
	}
	if !strings.Contains(c.Message, "1 of 5 selected") {
		t.Errorf("message %q should count members against selected pods", c.Message)
	}
}

func TestBindingStatus_SelectedButNoMembers(t *testing.T) {
	got := reconcileBinding(t, NewNamespaceFilter(nil), webBinding("payments", ""),
		mkNamespace("webapp", nil), mkVnet("payments", "webapp", nil),
		selectedPod("a", "none", true), selectedPod("b", "", false))

	if len(got.Status.AttachedPods) != 0 {
		t.Errorf("attachedPods = %v, want none", got.Status.AttachedPods)
	}
	c := readyCondition(t, got)
	if c.Reason != ReasonBindingNoPodsAttached {
		t.Errorf("Ready reason = %s, want %s (message %q)", c.Reason, ReasonBindingNoPodsAttached, c.Message)
	}
	if !strings.Contains(c.Message, "2 pod(s) match") {
		t.Errorf("message %q should count the selected pods", c.Message)
	}
}

// A vnet whose home namespace is unmanaged is not served: the vnet reconciler
// deletes its membership policies. A binding to it must not claim success.
func TestBindingStatus_HomeNamespaceNotManaged(t *testing.T) {
	permitsWebapp := &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp"}}
	for _, tc := range []struct {
		name    string
		filter  *NamespaceFilter
		homeNS  client.Object // nil: the namespace does not exist
		wantRsn string
	}{
		{"disabled annotation", NewNamespaceFilter(nil),
			testutil.Namespace("platform", map[string]string{AnnotationDisabled: "true"}, nil),
			ReasonBindingHomeNamespaceExcluded},
		{"excluded list", NewNamespaceFilter([]string{"platform"}),
			mkNamespace("platform", nil), ReasonBindingHomeNamespaceExcluded},
		{"missing", NewNamespaceFilter(nil), nil, ReasonBindingHomeNamespaceExcluded},
		{"managed", NewNamespaceFilter(nil), mkNamespace("platform", nil), ReasonBindingPodsAttached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: "webapp", Name: "web-0",
				Labels:      map[string]string{"app": "web", SystemLabelKey("platform", "payments"): "both"},
				Annotations: map[string]string{AnnotationResolvedGeneration: "0"},
			}}
			objs := []client.Object{mkNamespace("webapp", nil), mkVnet("payments", "platform", permitsWebapp), pod}
			if tc.homeNS != nil {
				objs = append(objs, tc.homeNS)
			}
			got := reconcileBinding(t, tc.filter, webBinding("payments", "platform"), objs...)
			c := readyCondition(t, got)
			if c.Reason != tc.wantRsn {
				t.Fatalf("Ready reason = %s, want %s (message %q)", c.Reason, tc.wantRsn, c.Message)
			}
			if tc.wantRsn == ReasonBindingHomeNamespaceExcluded {
				if c.Status != metav1.ConditionFalse {
					t.Errorf("Ready = %s, want False", c.Status)
				}
				if len(got.Status.AttachedPods) != 0 {
					t.Errorf("attachedPods = %v, want none", got.Status.AttachedPods)
				}
			}
		})
	}
}

func TestBindingStatus_VnetTerminating(t *testing.T) {
	vnet := mkVnet("payments", "webapp", nil)
	now := metav1.Now()
	vnet.DeletionTimestamp = &now
	vnet.Finalizers = []string{"test/hold"}
	got := reconcileBinding(t, NewNamespaceFilter(nil), webBinding("payments", ""),
		mkNamespace("webapp", nil), vnet, selectedPod("a", "both", true))
	c := readyCondition(t, got)
	if c.Status != metav1.ConditionFalse || c.Reason != ReasonBindingVNetTerminating {
		t.Fatalf("Ready = %s/%s, want False/%s", c.Status, c.Reason, ReasonBindingVNetTerminating)
	}
}
