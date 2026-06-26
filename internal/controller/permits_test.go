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

// schemeForPermits builds the minimal scheme Permits' fake client needs.
func schemeForPermits(t *testing.T) *runtime.Scheme {
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

func mkVnet(name, namespace string, allowed *vnetv1alpha1.NamespaceSelector) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       vnetv1alpha1.VirtualNetworkSpec{AllowedNamespaces: allowed},
	}
}

func mkNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}
}

func TestPermits(t *testing.T) {
	tests := []struct {
		name     string
		objects  []runtime.Object
		vnetKey  VnetKey
		podNS    string
		want     bool
		wantErr  bool
	}{
		{
			name:    "cluster_vnet_always_permitted",
			vnetKey: VnetKey(SystemVnetCluster),
			podNS:   "any-ns",
			want:    true,
		},
		{
			name:    "cluster_vnet_always_permitted_even_without_cr",
			vnetKey: VnetKey(SystemVnetCluster),
			podNS:   "ns-x",
			want:    true,
		},
		{
			name:    "vnet_not_found",
			vnetKey: VnetKey("home.missing"),
			podNS:   "any-ns",
			want:    false,
		},
		{
			name:    "malformed_key_empty",
			vnetKey: VnetKey(""),
			podNS:   "any-ns",
			want:    false,
		},
		{
			name:    "malformed_key_no_dot",
			vnetKey: VnetKey("noseparator"),
			podNS:   "any-ns",
			want:    false,
		},
		{
			name:    "home_NS_always_permitted",
			objects: []runtime.Object{mkVnet("payments", "platform", nil)},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "platform",
			want:    true,
		},
		{
			// Regression: prior implementation short-circuited on home-NS
			// before verifying vnet existence, causing the stamp to lie
			// for pod-labels referencing non-existent vnets in the pod's
			// own NS. Vnet missing → not permitted, even from home NS.
			name:    "home_NS_but_vnet_missing",
			objects: nil,
			vnetKey: VnetKey("platform.payments"),
			podNS:   "platform",
			want:    false,
		},
		{
			name:    "non_home_NS_with_nil_allowed",
			objects: []runtime.Object{mkVnet("payments", "platform", nil)},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "webapp",
			want:    false,
		},
		{
			name: "non_home_NS_with_all_true",
			objects: []runtime.Object{
				mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{All: true}),
			},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "webapp",
			want:    true,
		},
		{
			name: "non_home_NS_in_names_list",
			objects: []runtime.Object{
				mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp", "dashboard"}}),
			},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "webapp",
			want:    true,
		},
		{
			name: "non_home_NS_not_in_names_list",
			objects: []runtime.Object{
				mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{Names: []string{"webapp"}}),
			},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "intruder",
			want:    false,
		},
		{
			name: "non_home_NS_matching_selector",
			objects: []runtime.Object{
				mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
				mkNamespace("api", map[string]string{"tier": "prod"}),
			},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "api",
			want:    true,
		},
		{
			name: "non_home_NS_non_matching_selector",
			objects: []runtime.Object{
				mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
				mkNamespace("dev-api", map[string]string{"tier": "dev"}),
			},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "dev-api",
			want:    false,
		},
		{
			name: "non_home_NS_selector_NS_object_missing",
			// Selector path but the namespace object itself isn't in the cache.
			// Falls through to "no match" — not an error.
			objects: []runtime.Object{
				mkVnet("payments", "platform", &vnetv1alpha1.NamespaceSelector{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
			},
			vnetKey: VnetKey("platform.payments"),
			podNS:   "ghost",
			want:    false,
		},
		{
			name: "per_NS_namespace_vnet_only_home",
			// The per-NS `namespace` system vnet has AllowedNamespaces=nil.
			// Only pods in headlamp can join headlamp.namespace.
			objects: []runtime.Object{mkVnet("namespace", "headlamp", nil)},
			vnetKey: VnetKey("headlamp.namespace"),
			podNS:   "webapp",
			want:    false,
		},
		{
			name:    "per_NS_namespace_vnet_home_pod",
			objects: []runtime.Object{mkVnet("namespace", "headlamp", nil)},
			vnetKey: VnetKey("headlamp.namespace"),
			podNS:   "headlamp",
			want:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := schemeForPermits(t)
			b := fake.NewClientBuilder().WithScheme(scheme)
			if tt.objects != nil {
				b = b.WithRuntimeObjects(tt.objects...)
			}
			c := b.Build()
			got, err := Permits(context.Background(), c, tt.vnetKey, tt.podNS)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected err: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitVnetKey(t *testing.T) {
	cases := []struct {
		k        VnetKey
		homeNS   string
		vnetName string
		ok       bool
	}{
		{VnetKey(""), "", "", false},
		{VnetKey("cluster"), "", "cluster", true},
		{VnetKey("noseparator"), "", "", false},
		{VnetKey(".dotleading"), "", "", false},
		{VnetKey("trailing."), "", "", false},
		{VnetKey("platform.payments"), "platform", "payments", true},
		{VnetKey("headlamp.namespace"), "headlamp", "namespace", true},
		// First-dot-wins for vnets with dots in name (unlikely but representable).
		{VnetKey("ns.a.b"), "ns", "a.b", true},
	}
	for _, c := range cases {
		t.Run(string(c.k), func(t *testing.T) {
			home, name, ok := splitVnetKey(c.k)
			if home != c.homeNS || name != c.vnetName || ok != c.ok {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", home, name, ok, c.homeNS, c.vnetName, c.ok)
			}
		})
	}
}
