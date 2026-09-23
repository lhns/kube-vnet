package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func reconcileName(t *testing.T, r reconcileFunc, ns, name string) error {
	t.Helper()
	_, err := r(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	return err
}

type reconcileFunc func(context.Context, ctrl.Request) (ctrl.Result, error)

// A failed baseline apply leaves the namespace without default-deny; its
// owner must hear about it in the namespace, not only in operator logs.
func TestNamespaceReconciler_BaselineApplyFailureWarnsInNamespace(t *testing.T) {
	c, _ := patchCountingClient(t, "app", mkNamespace("app", nil))
	rec := &fakeRecorder{}
	r := &NamespaceReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}

	if err := reconcileName(t, r.Reconcile, "", "app"); err == nil {
		t.Fatal("want the apply error returned for a retry")
	}
	got := rec.on(EventApplyFailed, &networkingv1.NetworkPolicy{}, "app", BaselinePolicyName)
	if len(got) != 1 || !strings.Contains(got[0], "fail-open") {
		t.Fatalf("ApplyFailed on the baseline = %q, want one saying the namespace fails open", got)
	}
}

// The baseline's first creation is not a restore; recreating it after a
// delete is, and is reported on the policy. Recreating it after the
// operator itself swept it (namespace disabled, then re-enabled) is not.
func TestNamespaceReconciler_BaselineRestore(t *testing.T) {
	ctx := context.Background()
	ns := mkNamespace("app", nil)
	c, _ := patchCountingClient(t, "", ns)
	rec := &fakeRecorder{}
	r := &NamespaceReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}
	restores := func() int {
		return len(rec.on(EventPolicyRestored, &networkingv1.NetworkPolicy{}, "app", BaselinePolicyName))
	}
	deleteBaseline := func() {
		t.Helper()
		p := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: BaselinePolicyName}}
		if err := c.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if err := reconcileName(t, r.Reconcile, "", "app"); err != nil || restores() != 0 {
		t.Fatalf("first creation: err=%v restores=%d", err, restores())
	}
	deleteBaseline()
	if err := reconcileName(t, r.Reconcile, "", "app"); err != nil || restores() != 1 {
		t.Fatalf("after a delete: err=%v restores=%d, want 1", err, restores())
	}

	r.NSFilter = NewNamespaceFilter([]string{"app"})
	if err := reconcileName(t, r.Reconcile, "", "app"); err != nil {
		t.Fatal(err)
	}
	r.NSFilter = NewNamespaceFilter(nil)
	if err := reconcileName(t, r.Reconcile, "", "app"); err != nil || restores() != 1 {
		t.Fatalf("re-enabled namespace: err=%v restores=%d, want still 1", err, restores())
	}
}

func TestSystemVnetReconciler_CreateFailureWarnsInNamespace(t *testing.T) {
	c, _ := patchCountingClient(t, "app", mkNamespace("app", nil))
	rec := &fakeRecorder{}
	r := &SystemVnetReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}

	if err := reconcileName(t, r.Reconcile, "", "app"); err == nil {
		t.Fatal("want the apply error returned for a retry")
	}
	if got := rec.on(EventApplyFailed, &vnetv1alpha1.VirtualNetwork{}, "app", SystemVnetNamespace); len(got) != 1 {
		t.Fatalf("ApplyFailed on the system vnet = %q, want 1", got)
	}
}

// A failed host-port policy is reported on that policy, and doesn't stop the
// other ports' policies.
func TestHostPortReconciler_ApplyFailureWarnsOnPolicy(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "hp"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: []corev1.ContainerPort{
			{HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolTCP},
			{HostPort: 8081, ContainerPort: 81, Protocol: corev1.ProtocolTCP},
		}}}},
	}
	c, patches := patchCountingClient(t, "app", mkNamespace("app", nil), pod)
	rec := &fakeRecorder{}
	r := &HostPortReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}

	if err := reconcileName(t, r.Reconcile, "", "app"); err == nil {
		t.Fatal("want the apply errors returned")
	}
	if *patches != 2 {
		t.Errorf("patches = %d, want both ports tried", *patches)
	}
	for _, port := range []int32{8080, 8081} {
		name := hostPortPolicyName("app", hostPortKey{port: port, protocol: corev1.ProtocolTCP})
		if got := rec.on(EventApplyFailed, &networkingv1.NetworkPolicy{}, "app", name); len(got) != 1 {
			t.Errorf("ApplyFailed on %s = %q, want 1", name, got)
		}
	}
}

func exposedService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", UID: "web-uid"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP}},
		},
	}
}

func TestExternalAllow_ApplyFailureWarnsOnService(t *testing.T) {
	c, _ := patchCountingClient(t, "app", mkNamespace("app", nil), exposedService())
	rec := &fakeRecorder{}
	r := &ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: rec}

	if err := reconcileName(t, r.Reconcile, "app", "web"); err == nil {
		t.Fatal("want the apply error returned")
	}
	if got := rec.on(EventApplyFailed, &corev1.Service{}, "app", "web"); len(got) != 1 {
		t.Fatalf("ApplyFailed on the Service = %q, want 1", got)
	}
}

// A deleted external-allow policy is recreated with a PolicyRestored on it;
// one the operator swept (opt-out, then opt back in) is not a restore.
func TestExternalAllow_Restore(t *testing.T) {
	ctx := context.Background()
	c, _ := patchCountingClient(t, "", mkNamespace("app", nil), exposedService())
	rec := &fakeRecorder{}
	r := &ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: rec}
	name := externalAllowPolicyName(exposedService())
	restores := func() int { return len(rec.on(EventPolicyRestored, &networkingv1.NetworkPolicy{}, "app", name)) }
	setOptOut := func(v string) {
		t.Helper()
		svc := &corev1.Service{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "app", Name: "web"}, svc); err != nil {
			t.Fatal(err)
		}
		svc.Annotations = map[string]string{AnnotationExternalAllow: v}
		if err := c.Update(ctx, svc); err != nil {
			t.Fatal(err)
		}
	}

	if err := reconcileName(t, r.Reconcile, "app", "web"); err != nil || restores() != 0 {
		t.Fatalf("first creation: err=%v restores=%d", err, restores())
	}
	p := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name}}
	if err := c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := reconcileName(t, r.Reconcile, "app", "web"); err != nil || restores() != 1 {
		t.Fatalf("after a delete: err=%v restores=%d, want 1", err, restores())
	}

	setOptOut("false")
	if err := reconcileName(t, r.Reconcile, "app", "web"); err != nil {
		t.Fatal(err)
	}
	setOptOut("true")
	if err := reconcileName(t, r.Reconcile, "app", "web"); err != nil || restores() != 1 {
		t.Fatalf("after opting back in: err=%v restores=%d, want still 1", err, restores())
	}
}

// reconcilePod runs the resolution reconciler on pod (namespace "app") twice
// and returns what it recorded.
func reconcilePod(t *testing.T, pod *corev1.Pod, managed, waitEnabled bool) *fakeRecorder {
	t.Helper()
	pod.Namespace, pod.Name, pod.UID = "app", "p", "p-uid"
	ns := mkNamespace("app", nil)
	if !managed {
		ns.Annotations = map[string]string{AnnotationDisabled: "true"}
	}
	c := fake.NewClientBuilder().WithScheme(resolutionSchemeForTest(t)).WithObjects(ns, pod).Build()
	rec := &fakeRecorder{}
	r := &ResolutionReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec, NetworkWaitEnabled: waitEnabled}
	for range 2 {
		if err := reconcileName(t, r.Reconcile, "app", "p"); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

func waitPod(value string, injected bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationNetworkMaxWait: value}}}
	if injected {
		p.Spec.InitContainers = []corev1.Container{{Name: NetworkWaitContainerName}}
	}
	return p
}

// The webhook's network-wait warnings reach only a pod's direct creator. The
// same cases, plus a wait the webhook failed to inject, become one pod Event.
func TestResolution_NetworkWaitSkippedOncePerPod(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		pod                  *corev1.Pod
		managed, waitEnabled bool
		want                 string // "" means no Event
	}{
		{"injected", waitPod("30s", true), true, true, ""},
		{"invalid value", waitPod("soon", false), true, true, "not a positive duration"},
		{"feature disabled", waitPod("30s", false), true, false, "not enabled on this cluster"},
		{"unmanaged namespace", waitPod("30s", false), false, true, "does not manage this namespace"},
		{"not injected", waitPod("30s", false), true, true, "did not inject"},
		{"not asked", &corev1.Pod{}, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := reconcilePod(t, tc.pod, tc.managed, tc.waitEnabled)
			got := rec.on(ReasonNetworkWaitSkipped, &corev1.Pod{}, "app", "p")
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no Event, got %q", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("want one Event containing %q over two reconciles, got %q", tc.want, got)
			}
		})
	}
}

// A join label in an unmanaged namespace has no effect; its pod is told once.
// Pods there without one stay quiet.
func TestResolution_NamespaceNotManagedWarnsOnJoinLabel(t *testing.T) {
	rec := reconcilePod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"kube-vnet/net.web": "both"},
	}}, false, false)
	got := rec.on(ReasonNamespaceNotManaged, &corev1.Pod{}, "app", "p")
	if len(got) != 1 || !strings.Contains(got[0], "kube-vnet/net.web") {
		t.Fatalf("want one NamespaceNotManaged naming the label, got %q", got)
	}

	rec = reconcilePod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}}}, false, false)
	if len(rec.reasons) != 0 {
		t.Fatalf("pod without a join label warned: %v", rec.notes)
	}
}
