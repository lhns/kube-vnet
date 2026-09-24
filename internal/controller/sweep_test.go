package controller

import (
	"context"
	"maps"
	"slices"
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
			"kube-vnet.system/net.old":             "both",    // managed, will be removed
			"kube-vnet.system/net.payments":        "ingress", // managed, will be updated
			"kube-vnet.system/host-port.stale.tcp": "true",    // managed (different prefix), will be removed
			"app":                                  "demo",    // unmanaged, untouched
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

// serviceSource.sweep: ownership by controller owner reference

func mkPolicy(name string, labels map[string]string, owner *metav1.OwnerReference) *networkingv1.NetworkPolicy {
	pol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", Labels: labels}}
	if owner != nil {
		pol.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return pol
}

// sweepWeb runs the Service-source sweep for Service ns1/web over objs,
// keeping the policy named keep, and returns the names left.
func sweepWeb(t *testing.T, keep string, objs ...client.Object) []string {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = networkingv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	web := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "web", UID: "svc-web-uid"}}
	if err := svcSourcePolicies.sweep(context.Background(), c, web, keep); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var list networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	var left []string
	for _, p := range list.Items {
		left = append(left, p.Name)
	}
	slices.Sort(left)
	return left
}

func TestServiceSourceSweep(t *testing.T) {
	webRef := &metav1.OwnerReference{APIVersion: "v1", Kind: "Service", Name: "web", UID: "svc-web-uid", Controller: ptr(true)}
	otherRef := &metav1.OwnerReference{Kind: "Service", Name: "other", UID: "other-uid", Controller: ptr(true)}
	managed := map[string]string{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow}
	withKind := func(kind string) map[string]string {
		l := maps.Clone(managed)
		l[LabelSourceKind] = kind
		return l
	}

	cases := []struct {
		name string
		keep string
		objs []client.Object
		want []string
	}{
		{
			// A legacy name is swept, the current one kept, another Service's
			// policy left alone.
			name: "legacy_migration",
			keep: "current",
			objs: []client.Object{
				mkPolicy("legacy", managed, webRef),
				mkPolicy("current", managed, webRef),
				mkPolicy("other", managed, otherRef),
			},
			want: []string{"current", "other"},
		},
		{
			name: "empty_keep_deletes_all",
			objs: []client.Object{mkPolicy("legacy", managed, webRef), mkPolicy("current", managed, webRef)},
		},
		{
			// Regression: the Service-source sweep deleted the
			// apiserver-reachable policy on every reconcile of a webhook
			// Service that isn't externally exposed.
			name: "spares_other_source_kind",
			objs: []client.Object{
				mkPolicy("svc", withKind(LabelSourceKindService), webRef),
				mkPolicy("apiserver", withKind(LabelSourceKindApiserver), webRef),
				mkPolicy("legacy-no-kind", managed, webRef),
			},
			want: []string{"apiserver"},
		},
		{
			// Host-source policies carry no owner reference.
			name: "skips_policies_without_owner",
			objs: []client.Object{mkPolicy("host", managed, nil)},
			want: []string{"host"},
		},
		{
			// LabelK8sManagedBy is user-writable, so it is never an ownership
			// signal, even with a matching owner reference.
			name: "standard_managed_by_label_alone",
			objs: []client.Object{mkPolicy("user-policy", map[string]string{
				LabelK8sManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow,
			}, webRef)},
			want: []string{"user-policy"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sweepWeb(t, c.keep, c.objs...); !slices.Equal(got, c.want) {
				t.Errorf("left %v, want %v", got, c.want)
			}
		})
	}
}

// The label-based sweep never matches a policy carrying only the standard
// managed-by label.
func TestSweepStalePolicies_StandardManagedByLabelAlone(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = networkingv1.AddToScheme(scheme)
	impostor := mkPolicy("user-policy", map[string]string{
		LabelK8sManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow,
	}, nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(impostor).Build()
	if err := sweepStalePolicies(context.Background(), c,
		inNamespacePolicyLabels("ns1", map[string]string{LabelRole: LabelRoleExternalAllow}), nil, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(impostor), &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("label-based sweep deleted a policy that carries only the standard managed-by label: %v", err)
	}
}
