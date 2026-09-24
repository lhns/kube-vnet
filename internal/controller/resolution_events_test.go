package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// reconcileWithEvents runs the resolution reconciler on pod `p` in namespace
// "app" and returns what it recorded plus the stamps it left on the pod.
func reconcileWithEvents(t *testing.T, labels map[string]string, extra ...client.Object) (*fakeRecorder, map[string]string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "p", Labels: labels}}
	objs := append([]client.Object{
		mkNamespace("app", nil),
		mkVnet("web", "app", nil),
		pod,
	}, extra...)
	c := fake.NewClientBuilder().WithScheme(resolutionSchemeForTest(t)).WithObjects(objs...).Build()
	rec := &fakeRecorder{}
	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "app", Name: "p"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	stamps := map[string]string{}
	for k, v := range got.Labels {
		if IsResolutionManagedLabel(k) {
			stamps[k] = v
		}
	}
	return rec, stamps
}

// A binding and a pod label that disagree intersect to none. The pod is
// correctly left out of the vnet; the Event is what tells anyone why.
func TestResolutionEvents_ConflictWarnsOnPod(t *testing.T) {
	binding := &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "b"},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "web"},
			Direction:         "egress",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"tier": "front"}},
		},
	}
	rec, stamps := reconcileWithEvents(t,
		map[string]string{"tier": "front", "kube-vnet/net.web": "ingress"}, binding)

	if len(stamps) != 0 {
		t.Fatalf("ingress ∩ egress must leave the pod out of the vnet, got stamps %v", stamps)
	}
	note := only(t, rec, ReasonResolutionConflict)
	for _, want := range []string{"VirtualNetworkBinding/b=egress", "pod label kube-vnet/net.web=ingress", "app/web", "not a member"} {
		if !strings.Contains(note, want) {
			t.Errorf("conflict message %q should mention %q", note, want)
		}
	}
}

// A bare cluster-baseline value is a pin; a pod label cannot change it.
func TestResolutionEvents_OverrideRejectedWarnsOnPod(t *testing.T) {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{
			Memberships: []vnetv1alpha1.BaselineMembership{{
				VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "web"},
				Direction:         "both",
			}},
		},
	}
	rec, stamps := reconcileWithEvents(t, map[string]string{"kube-vnet/net.web": "ingress"}, cb)

	if got := stamps[LabelSystemNetPrefix+"app.web"]; got != "both" {
		t.Fatalf("the pinned value must win, got %q", got)
	}
	note := only(t, rec, ReasonOverrideRejected)
	for _, want := range []string{"app/web", "a binding or pod label", `"ingress"`, "the ClusterVirtualNetworkBaseline", `"both"`, "default-*"} {
		if !strings.Contains(note, want) {
			t.Errorf("override message %q should mention %q", note, want)
		}
	}
}

// Rules that agree produce no Warning. Without this the two tests above could
// pass on a reconciler that warns about everything.
func TestResolutionEvents_NoWarningWithoutDisagreement(t *testing.T) {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{
			Memberships: []vnetv1alpha1.BaselineMembership{{
				VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "web"},
				Direction:         "default-both",
			}},
		},
	}
	rec, stamps := reconcileWithEvents(t, map[string]string{"kube-vnet/net.web": "ingress"}, cb)

	if got := stamps[LabelSystemNetPrefix+"app.web"]; got != "ingress" {
		t.Fatalf("a default-* baseline must yield to the pod label, got %q", got)
	}
	if len(rec.reasons) != 0 {
		t.Fatalf("no disagreement, but got events %v: %v", rec.reasons, rec.notes)
	}
}

// An Event on the cluster-scoped baseline would land in `default` and name the
// pod's namespace there; it goes on the pod instead.
func TestResolutionEvents_ClusterBaselineNotJoinableGoesOnPod(t *testing.T) {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{
			Memberships: []vnetv1alpha1.BaselineMembership{{
				VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "missing", Namespace: "elsewhere"},
				Direction:         "both",
			}},
		},
	}
	rec, _ := reconcileWithEvents(t, nil, cb)

	only(t, rec, ReasonVirtualNetworkNotJoinable)
	if got := rec.on(ReasonVirtualNetworkNotJoinable, &corev1.Pod{}, "app", "p"); len(got) != 1 ||
		!strings.Contains(got[0], "ClusterVirtualNetworkBaseline/default") {
		t.Fatalf("want the Event on the pod, naming the baseline as source; got %q", got)
	}
}

func only(t *testing.T, rec *fakeRecorder, reason string) string {
	t.Helper()
	if len(rec.reasons) != 1 || rec.reasons[0] != reason {
		t.Fatalf("want exactly one %s event, got %v: %v", reason, rec.reasons, rec.notes)
	}
	return rec.notes[0]
}
