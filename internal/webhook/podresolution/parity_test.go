package podresolution

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/controller"
)

// The webhook and the reconciler must produce the same membership stamps for
// the same pod. They share controller.Resolver precisely so they cannot
// disagree; this test is what keeps that true. Reimplement resolution in the
// handler and every case below breaks.
//
// The comparison runs the two real entry points, not the shared helper:
// ResolutionReconciler.Reconcile against a fake client, and Mutator.Handle
// with its JSON patch actually applied. A wiring mistake in either path shows
// up here even though the resolution logic itself is shared.
func TestWebhookAndReconcilerAgree(t *testing.T) {
	const podNS = "app"

	for _, tc := range []struct {
		name    string
		objects []client.Object
		pod     *corev1.Pod
		want    map[string]string
	}{
		{
			name:    "bare pod join label",
			objects: []client.Object{vnet("web", podNS, nil)},
			pod:     pod(podNS, map[string]string{"kube-vnet/net.web": "both"}),
			want:    map[string]string{"kube-vnet.system/net.app.web": "both"},
		},
		{
			name: "binding with matchExpressions",
			objects: []client.Object{
				vnet("web", podNS, nil),
				&vnetv1alpha1.VirtualNetworkBinding{
					ObjectMeta: metav1.ObjectMeta{Namespace: podNS, Name: "b"},
					Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
						VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "web"},
						Direction:         "ingress",
						PodSelector: metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{{
								Key:      "tier",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"front", "edge"},
							}},
						},
					},
				},
			},
			pod:  pod(podNS, map[string]string{"tier": "edge"}),
			want: map[string]string{"kube-vnet.system/net.app.web": "ingress"},
		},
		{
			name: "namespace baseline",
			objects: []client.Object{
				vnet("web", podNS, nil),
				&vnetv1alpha1.VirtualNetworkBaseline{
					ObjectMeta: metav1.ObjectMeta{Namespace: podNS, Name: "default"},
					Spec: vnetv1alpha1.VirtualNetworkBaselineSpec{
						Memberships: []vnetv1alpha1.BaselineMembership{{
							VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "web"},
							Direction:         "both",
						}},
					},
				},
			},
			pod:  pod(podNS, nil),
			want: map[string]string{"kube-vnet.system/net.app.web": "both"},
		},
		{
			name: "cluster baseline overridden by pod label",
			objects: []client.Object{
				vnet("web", podNS, nil),
				&vnetv1alpha1.ClusterVirtualNetworkBaseline{
					ObjectMeta: metav1.ObjectMeta{Name: "default"},
					Spec: vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{
						Memberships: []vnetv1alpha1.BaselineMembership{{
							VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "web"},
							Direction:         "default-both",
						}},
					},
				},
			},
			pod:  pod(podNS, map[string]string{"kube-vnet/net.web": "ingress"}),
			want: map[string]string{"kube-vnet.system/net.app.web": "ingress"},
		},
		{
			name: "cross-namespace ref, permitted",
			objects: []client.Object{
				ns("other"),
				vnet("shared", "other", &vnetv1alpha1.NamespaceSelector{All: true}),
			},
			pod:  pod(podNS, map[string]string{"kube-vnet/net.other.shared": "both"}),
			want: map[string]string{"kube-vnet.system/net.other.shared": "both"},
		},
		{
			name: "cross-namespace ref, not permitted",
			objects: []client.Object{
				ns("other"),
				vnet("shared", "other", nil), // home namespace only
			},
			pod:  pod(podNS, map[string]string{"kube-vnet/net.other.shared": "both"}),
			want: map[string]string{},
		},
		{
			name:    "unknown vnet is not stamped",
			objects: nil,
			pod:     pod(podNS, map[string]string{"kube-vnet/net.ghost": "both"}),
			want:    map[string]string{},
		},
		{
			name:    "direction none yields no stamp",
			objects: []client.Object{vnet("web", podNS, nil)},
			pod:     pod(podNS, map[string]string{"kube-vnet/net.web": "none"}),
			want:    map[string]string{},
		},
		{
			name:    "hostPort stamp",
			objects: nil,
			pod:     hostPortPod(podNS, false),
			want:    map[string]string{"kube-vnet.system/host-port.8080.tcp": "true"},
		},
		{
			name:    "hostNetwork pod gets no hostPort stamp",
			objects: nil,
			pod:     hostPortPod(podNS, true),
			want:    map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viaController := reconcilerLabels(t, tc.objects, tc.pod)
			viaWebhook := mutatorLabels(t, tc.objects, tc.pod)

			if !reflect.DeepEqual(viaController, tc.want) {
				t.Errorf("reconciler stamped %v, want %v", viaController, tc.want)
			}
			if !reflect.DeepEqual(viaWebhook, tc.want) {
				t.Errorf("webhook stamped %v, want %v", viaWebhook, tc.want)
			}
			if !reflect.DeepEqual(viaController, viaWebhook) {
				t.Errorf("paths disagree: reconciler %v, webhook %v. The admission and "+
					"controller paths must resolve identically, or a pod's membership "+
					"depends on which one reached it first", viaController, viaWebhook)
			}
		})
	}
}

// reconcilerLabels runs the real reconciler and reads the stamps back off the pod.
func reconcilerLabels(t *testing.T, objects []client.Object, p *corev1.Pod) map[string]string {
	t.Helper()
	c := newClient(t, objects, p)
	r := &controller.ResolutionReconciler{
		Client:   c,
		Scheme:   testScheme(t),
		NSFilter: controller.NewNamespaceFilter(nil),
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return managedLabels(&got)
}

// mutatorLabels runs the real handler and applies its JSON patch, so a
// mis-built patch fails here rather than passing silently.
func mutatorLabels(t *testing.T, objects []client.Object, p *corev1.Pod) map[string]string {
	t.Helper()
	scheme := testScheme(t)
	c := newClient(t, objects, p)
	nsFilter := controller.NewNamespaceFilter(nil)
	m := &Mutator{
		Resolver: &controller.Resolver{Reader: c, NSFilter: nsFilter},
		Reader:   c,
		NSFilter: nsFilter,
		Decoder:  admission.NewDecoder(scheme),
	}

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	resp := m.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: p.Namespace,
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
	if !resp.Allowed {
		t.Fatalf("mutator denied the pod: %+v", resp.Result)
	}

	patched := raw
	if len(resp.Patches) > 0 {
		ops, err := json.Marshal(resp.Patches)
		if err != nil {
			t.Fatalf("marshal patches: %v", err)
		}
		patch, err := jsonpatch.DecodePatch(ops)
		if err != nil {
			t.Fatalf("decode patch %s: %v", ops, err)
		}
		if patched, err = patch.Apply(raw); err != nil {
			t.Fatalf("apply patch %s: %v", ops, err)
		}
	}
	var out corev1.Pod
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatalf("unmarshal patched pod: %v", err)
	}
	return managedLabels(&out)
}

func newClient(t *testing.T, objects []client.Object, p *corev1.Pod) client.Client {
	t.Helper()
	objs := append([]client.Object{ns(p.Namespace), p.DeepCopy()}, objects...)
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("admissionv1: %v", err)
	}
	if err := vnetv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("vnetv1alpha1: %v", err)
	}
	return s
}

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func vnet(name, namespace string, allowed *vnetv1alpha1.NamespaceSelector) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       vnetv1alpha1.VirtualNetworkSpec{AllowedNamespaces: allowed},
	}
}

func pod(namespace string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: "p", Labels: labels,
	}}
}

func hostPortPod(namespace string, hostNetwork bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "p"},
		Spec: corev1.PodSpec{
			HostNetwork: hostNetwork,
			Containers: []corev1.Container{{
				Name:  "c",
				Ports: []corev1.ContainerPort{{HostPort: 8080, ContainerPort: 8080}},
			}},
		},
	}
}
