package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSyncManagedLabels_AddsAndRemoves(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"kube-vnet.system/net.old":       "both",     // managed, will be removed
			"kube-vnet.system/net.payments":  "ingress",  // managed, will be updated
			"kube-vnet.system/host-port.stale.tcp": "true", // managed (different prefix), will be removed
			"app":                            "demo",     // unmanaged, untouched
		}},
	}
	isManaged := func(k string) bool {
		return strings.HasPrefix(k, "kube-vnet.system/net.") ||
			strings.HasPrefix(k, "kube-vnet.system/host-port.")
	}
	desired := map[string]string{
		"kube-vnet.system/net.payments":     "both",   // update
		"kube-vnet.system/net.new":          "egress", // add
		"kube-vnet.system/host-port.80.tcp": "true",   // add (different prefix family)
	}
	changed := syncManagedLabels(pod, isManaged, desired)
	if !changed {
		t.Error("expected changed=true")
	}
	want := map[string]string{
		"app":                               "demo",
		"kube-vnet.system/net.payments":     "both",
		"kube-vnet.system/net.new":          "egress",
		"kube-vnet.system/host-port.80.tcp": "true",
	}
	for k, v := range want {
		if pod.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if _, ok := pod.Labels["kube-vnet.system/net.old"]; ok {
		t.Error("kube-vnet.system/net.old should have been removed")
	}
	if _, ok := pod.Labels["kube-vnet.system/host-port.stale.tcp"]; ok {
		t.Error("stale host-port label should have been removed")
	}
	if len(pod.Labels) != len(want) {
		t.Errorf("label set size = %d, want %d (%v)", len(pod.Labels), len(want), pod.Labels)
	}
}

func TestSyncManagedLabels_NoOp(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"kube-vnet.system/net.x": "both",
		"unmanaged":              "y",
	}}}
	isManaged := func(k string) bool { return strings.HasPrefix(k, "kube-vnet.system/") }
	desired := map[string]string{"kube-vnet.system/net.x": "both"}
	if changed := syncManagedLabels(pod, isManaged, desired); changed {
		t.Error("expected changed=false for no-op call")
	}
}

func TestSyncManagedLabels_RemoveAll(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"kube-vnet.system/net.a": "both",
		"kube-vnet.system/net.b": "ingress",
		"app":                    "demo",
	}}}
	isManaged := func(k string) bool { return strings.HasPrefix(k, "kube-vnet.system/") }
	// Empty desired → remove all managed labels.
	if changed := syncManagedLabels(pod, isManaged, nil); !changed {
		t.Error("expected changed=true")
	}
	if len(pod.Labels) != 1 || pod.Labels["app"] != "demo" {
		t.Errorf("expected only app=demo to remain, got %v", pod.Labels)
	}
}

func TestSyncManagedLabels_NilLabelsNoDesired(t *testing.T) {
	pod := &corev1.Pod{}
	isManaged := func(k string) bool { return true }
	if changed := syncManagedLabels(pod, isManaged, nil); changed {
		t.Error("expected changed=false for nil labels + nil desired")
	}
	if pod.Labels != nil {
		t.Error("nil labels should stay nil")
	}
}

func TestSyncManagedLabels_NilLabelsThenAdd(t *testing.T) {
	pod := &corev1.Pod{}
	isManaged := func(k string) bool { return true }
	desired := map[string]string{"kube-vnet.system/net.x": "both"}
	if changed := syncManagedLabels(pod, isManaged, desired); !changed {
		t.Error("expected changed=true")
	}
	if pod.Labels["kube-vnet.system/net.x"] != "both" {
		t.Errorf("expected label added, got %v", pod.Labels)
	}
}

// hasControllerOwner table-tests

func TestHasControllerOwner(t *testing.T) {
	uidA := types.UID("uid-a")
	uidB := types.UID("uid-b")
	truePtr := true
	falsePtr := false
	mk := func(refs ...metav1.OwnerReference) client.Object {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{OwnerReferences: refs}}
	}
	cases := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{"no_owner_refs", mk(), false},
		{"matching_controller", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: &truePtr}), true},
		{"mismatched_uid", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidB, Controller: &truePtr}), false},
		{"mismatched_name", mk(metav1.OwnerReference{Kind: "Service", Name: "other", UID: uidA, Controller: &truePtr}), false},
		{"mismatched_kind", mk(metav1.OwnerReference{Kind: "Pod", Name: "web", UID: uidA, Controller: &truePtr}), false},
		{"controller_false", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: &falsePtr}), false},
		{"controller_nil", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: nil}), false},
		{"multiple_refs_one_matches", mk(
			metav1.OwnerReference{Kind: "Pod", Name: "decoy", UID: uidB, Controller: &truePtr},
			metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: &truePtr},
		), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasControllerOwner(c.obj, "Service", "web", uidA); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// sweepStalePoliciesByOwner: realistic legacy-migration scenario

func mkPolicy(name, ns string, labels map[string]string, owner *metav1.OwnerReference) *networkingv1.NetworkPolicy {
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
	}
	if owner != nil {
		pol.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return pol
}

func TestSweepStalePoliciesByOwner_DeletesLegacyKeepsCurrent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)

	truePtr := true
	svcRef := &metav1.OwnerReference{
		APIVersion: "v1", Kind: "Service",
		Name: "web", UID: types.UID("svc-web-uid"),
		Controller: &truePtr,
	}
	managed := map[string]string{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow}

	// Three objects in the fake client:
	//   legacy:  current-keep-set MISS  → should be deleted
	//   current: in keep-set            → kept
	//   other:   different owner        → untouched
	legacy := mkPolicy("kube-vnet.external-web-deadbeef", "ns1", managed, svcRef)
	current := mkPolicy("kube-vnet.ext.svc.web-cafebabe", "ns1", managed, svcRef)
	other := mkPolicy("kube-vnet.ext.svc.other-feedface", "ns1", managed,
		&metav1.OwnerReference{Kind: "Service", Name: "other", UID: "other-uid", Controller: &truePtr})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(legacy, current, other).Build()

	keep := map[client.ObjectKey]bool{
		{Namespace: "ns1", Name: "kube-vnet.ext.svc.web-cafebabe"}: true,
	}
	err := sweepStalePoliciesByOwner(context.Background(), c,
		inNamespacePolicyLabels("ns1", map[string]string{LabelRole: LabelRoleExternalAllow}),
		"Service", "web", types.UID("svc-web-uid"),
		keep,
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// legacy: gone
	var got networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "kube-vnet.external-web-deadbeef"}, &got); err == nil {
		t.Errorf("legacy policy still exists; should have been swept")
	}
	// current: still there
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "kube-vnet.ext.svc.web-cafebabe"}, &got); err != nil {
		t.Errorf("current policy was deleted: %v", err)
	}
	// other: untouched
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "kube-vnet.ext.svc.other-feedface"}, &got); err != nil {
		t.Errorf("other-Service's policy was deleted: %v", err)
	}
}

func TestSweepStalePoliciesByOwner_EmptyKeepDeletesAll(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)

	truePtr := true
	svcRef := &metav1.OwnerReference{
		Kind: "Service", Name: "web", UID: types.UID("svc-web-uid"),
		Controller: &truePtr,
	}
	managed := map[string]string{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow}

	policies := []*networkingv1.NetworkPolicy{
		mkPolicy("kube-vnet.external-web-deadbeef", "ns1", managed, svcRef),
		mkPolicy("kube-vnet.ext.svc.web-cafebabe", "ns1", managed, svcRef),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policies[0], policies[1]).Build()

	err := sweepStalePoliciesByOwner(context.Background(), c,
		inNamespacePolicyLabels("ns1", map[string]string{LabelRole: LabelRoleExternalAllow}),
		"Service", "web", types.UID("svc-web-uid"),
		nil, // nuke-all
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var list networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &list, client.InNamespace("ns1")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected 0 policies remaining, got %d", len(list.Items))
	}
}

func TestSweepStalePoliciesByOwner_SkipsPoliciesWithoutOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)

	managed := map[string]string{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow}
	// Host-source policies don't carry per-resource owner refs; the
	// owner-ref sweeper must skip them so it doesn't accidentally claim
	// host-source policies during a Service reconcile.
	noOwner := mkPolicy("kube-vnet.ext.host.8080.tcp-abc", "ns1", managed, nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(noOwner).Build()

	err := sweepStalePoliciesByOwner(context.Background(), c,
		inNamespacePolicyLabels("ns1", map[string]string{LabelRole: LabelRoleExternalAllow}),
		"Service", "web", types.UID("svc-web-uid"),
		nil,
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var got networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "kube-vnet.ext.host.8080.tcp-abc"}, &got); err != nil {
		t.Errorf("no-owner policy was deleted: %v", err)
	}
}
