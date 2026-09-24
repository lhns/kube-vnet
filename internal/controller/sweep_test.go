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

func TestSyncManagedLabels(t *testing.T) {
	isManaged := func(k string) bool { return strings.HasPrefix(k, "kube-vnet.system/") }
	cases := []struct {
		name        string
		labels      map[string]string
		desired     map[string]string
		want        map[string]string
		wantChanged bool
	}{
		{
			name: "adds_updates_removes",
			labels: map[string]string{
				"kube-vnet.system/net.old":             "both",    // removed
				"kube-vnet.system/net.payments":        "ingress", // updated
				"kube-vnet.system/host-port.stale.tcp": "true",    // removed
				"app":                                  "demo",    // unmanaged, untouched
			},
			desired: map[string]string{
				"kube-vnet.system/net.payments":     "both",
				"kube-vnet.system/net.new":          "egress",
				"kube-vnet.system/host-port.80.tcp": "true",
			},
			want: map[string]string{
				"app":                               "demo",
				"kube-vnet.system/net.payments":     "both",
				"kube-vnet.system/net.new":          "egress",
				"kube-vnet.system/host-port.80.tcp": "true",
			},
			wantChanged: true,
		},
		{
			name:    "no_op",
			labels:  map[string]string{"kube-vnet.system/net.x": "both", "unmanaged": "y"},
			desired: map[string]string{"kube-vnet.system/net.x": "both"},
			want:    map[string]string{"kube-vnet.system/net.x": "both", "unmanaged": "y"},
		},
		{
			name:        "remove_all",
			labels:      map[string]string{"kube-vnet.system/net.a": "both", "kube-vnet.system/net.b": "ingress", "app": "demo"},
			want:        map[string]string{"app": "demo"},
			wantChanged: true,
		},
		{name: "nil_labels_nothing_desired"},
		{
			name:        "nil_labels_then_add",
			desired:     map[string]string{"kube-vnet.system/net.x": "both"},
			want:        map[string]string{"kube-vnet.system/net.x": "both"},
			wantChanged: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: c.labels}}
			if changed := syncManagedLabels(pod, isManaged, c.desired); changed != c.wantChanged {
				t.Errorf("changed = %v, want %v", changed, c.wantChanged)
			}
			if !maps.Equal(pod.Labels, c.want) || (c.want == nil) != (pod.Labels == nil) {
				t.Errorf("labels = %v, want %v", pod.Labels, c.want)
			}
		})
	}
}

// hasControllerOwner table-tests

func TestHasControllerOwner(t *testing.T) {
	uidA := types.UID("uid-a")
	uidB := types.UID("uid-b")
	mk := func(refs ...metav1.OwnerReference) client.Object {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{OwnerReferences: refs}}
	}
	cases := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{"no_owner_refs", mk(), false},
		{"matching_controller", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: new(true)}), true},
		{"mismatched_uid", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidB, Controller: new(true)}), false},
		{"mismatched_name", mk(metav1.OwnerReference{Kind: "Service", Name: "other", UID: uidA, Controller: new(true)}), false},
		{"mismatched_kind", mk(metav1.OwnerReference{Kind: "Pod", Name: "web", UID: uidA, Controller: new(true)}), false},
		{"controller_false", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: new(false)}), false},
		{"controller_nil", mk(metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: nil}), false},
		{"multiple_refs_one_matches", mk(
			metav1.OwnerReference{Kind: "Pod", Name: "decoy", UID: uidB, Controller: new(true)},
			metav1.OwnerReference{Kind: "Service", Name: "web", UID: uidA, Controller: new(true)},
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
	webRef := &metav1.OwnerReference{APIVersion: "v1", Kind: "Service", Name: "web", UID: "svc-web-uid", Controller: new(true)}
	otherRef := &metav1.OwnerReference{Kind: "Service", Name: "other", UID: "other-uid", Controller: new(true)}
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
