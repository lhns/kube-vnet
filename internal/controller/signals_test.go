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

// restoreSteps drives one policy through a sequence of steps and counts the
// PolicyRestored Events on it. Steps: "ok" and "fail" reconcile with the
// apply succeeding or failing, "delete" deletes the policy, "drop" makes it
// undesired and reconciles (the operator sweeps it), "want" makes it desired
// again.
type restoreSteps struct {
	c          client.Client
	rec        *fakeRecorder
	failing    *bool
	reconcile  reconcileFunc
	ns, name   string // the reconcile request
	policy     client.ObjectKey
	drop, want func()
}

func (s restoreSteps) run(t *testing.T, steps ...string) int {
	t.Helper()
	for _, step := range steps {
		var err error
		switch step {
		case "ok", "fail":
			*s.failing = step == "fail"
			err = reconcileName(t, s.reconcile, s.ns, s.name)
			*s.failing = false
			if (err != nil) != (step == "fail") {
				t.Fatalf("%v: step %q: err=%v", steps, step, err)
			}
			continue
		case "delete":
			err = s.c.Delete(context.Background(), &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: s.policy.Namespace, Name: s.policy.Name}})
		case "drop":
			s.drop()
			err = reconcileName(t, s.reconcile, s.ns, s.name)
		case "want":
			s.want()
		default:
			t.Fatalf("unknown step %q", step)
		}
		if err != nil {
			t.Fatalf("%v: step %q: %v", steps, step, err)
		}
	}
	return len(s.rec.on(EventPolicyRestored, &networkingv1.NetworkPolicy{}, s.policy.Namespace, s.policy.Name))
}

// Restore tracking outlives a failed apply: the policy is still wanted, so
// recreating it after a delete is a restore however many attempts it takes.
// A policy the operator swept as undesired comes back as new.
var restoreSequences = []struct {
	name  string
	steps []string
	want  int
}{
	{"delete, failed re-apply, re-apply", []string{"ok", "delete", "fail", "ok"}, 1},
	{"failed first apply", []string{"fail", "ok"}, 0},
	{"swept, then wanted again", []string{"ok", "drop", "want", "ok"}, 0},
	{"swept, then wanted again after a failed apply", []string{"ok", "drop", "want", "fail", "ok"}, 0},
}

func TestHostPortReconciler_RestoreTracking(t *testing.T) {
	pod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "hp"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: []corev1.ContainerPort{
				{HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolTCP},
			}}}},
		}
	}
	for _, tc := range restoreSequences {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			failing := false
			c, _ := patchFailingClient(t, func(client.Object) bool { return failing }, mkNamespace("app", nil), pod())
			rec := &fakeRecorder{}
			r := &HostPortReconciler{Client: c, NSFilter: NewNamespaceFilter(nil), Recorder: rec}
			s := restoreSteps{
				c: c, rec: rec, failing: &failing, reconcile: r.Reconcile, name: "app",
				policy: client.ObjectKey{Namespace: "app",
					Name: hostPortPolicyName("app", hostPortKey{port: 8080, protocol: corev1.ProtocolTCP})},
				drop: func() {
					if err := c.Delete(ctx, pod()); err != nil {
						t.Fatal(err)
					}
				},
				want: func() {
					if err := c.Create(ctx, pod()); err != nil {
						t.Fatal(err)
					}
				},
			}
			if got := s.run(t, tc.steps...); got != tc.want {
				t.Fatalf("PolicyRestored = %d, want %d", got, tc.want)
			}
		})
	}
}

// The Service-owned reconcilers share serviceSource.reconcile; ExternalAllow
// stands for both.
func TestExternalAllow_RestoreTracking(t *testing.T) {
	for _, tc := range restoreSequences {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			failing := false
			c, _ := patchFailingClient(t, func(client.Object) bool { return failing }, mkNamespace("app", nil), exposedService())
			rec := &fakeRecorder{}
			r := &ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: rec}
			annotate := func(a map[string]string) {
				t.Helper()
				svc := &corev1.Service{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: "app", Name: "web"}, svc); err != nil {
					t.Fatal(err)
				}
				svc.Annotations = a
				if err := c.Update(ctx, svc); err != nil {
					t.Fatal(err)
				}
			}
			s := restoreSteps{
				c: c, rec: rec, failing: &failing, reconcile: r.Reconcile, ns: "app", name: "web",
				policy: client.ObjectKey{Namespace: "app", Name: externalAllowPolicyName(exposedService())},
				drop:   func() { annotate(map[string]string{AnnotationExternalAllow: "false"}) },
				want:   func() { annotate(nil) },
			}
			if got := s.run(t, tc.steps...); got != tc.want {
				t.Fatalf("PolicyRestored = %d, want %d", got, tc.want)
			}
		})
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
func TestResolution_NamespaceExcludedWarnsOnJoinLabel(t *testing.T) {
	rec := reconcilePod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"kube-vnet/net.web": "both"},
	}}, false, false)
	got := rec.on(ReasonNamespaceExcluded, &corev1.Pod{}, "app", "p")
	if len(got) != 1 || !strings.Contains(got[0], "kube-vnet/net.web") {
		t.Fatalf("want one NamespaceExcluded naming the label, got %q", got)
	}

	rec = reconcilePod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}}}, false, false)
	if len(rec.reasons) != 0 {
		t.Fatalf("pod without a join label warned: %v", rec.notes)
	}
}

// A named targetPort without a backing pod is expected while pods start and
// retries every 30s: a Normal Event when the wait starts, not on each retry,
// and again after the port resolved and was lost.
func TestExternalAllow_NamedPortUnresolvedOnTransition(t *testing.T) {
	ctx := context.Background()
	svc := exposedService()
	svc.Spec.Ports[0].TargetPort = intstr.FromString("http")
	c, _ := patchCountingClient(t, "", mkNamespace("app", nil), svc)
	rec := &fakeRecorder{}
	r := &ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: rec}
	waits := func() []string { return rec.on(ReasonNamedPortUnresolved, &corev1.Service{}, "app", "web") }
	reconcile := func() {
		t.Helper()
		if err := reconcileName(t, r.Reconcile, "app", "web"); err != nil {
			t.Fatal(err)
		}
	}

	reconcile()
	reconcile()
	if got := waits(); len(got) != 1 {
		t.Fatalf("two retries of one wait: %d Events, want 1", len(got))
	}
	for i, reason := range rec.reasons {
		if reason == ReasonNamedPortUnresolved && rec.types[i] != corev1.EventTypeNormal {
			t.Fatalf("%s is %s, want Normal", reason, rec.types[i])
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-0", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if got := waits(); len(got) != 2 {
		t.Fatalf("a new wait after the port resolved: %d Events in total, want 2", len(got))
	}
}

// An exposed Service without a selector stays blocked until its owner acts.
func TestExternalAllow_ServiceHasNoSelectorWarns(t *testing.T) {
	svc := exposedService()
	svc.Spec.Selector = nil
	c, _ := patchCountingClient(t, "", mkNamespace("app", nil), svc)
	rec := &fakeRecorder{}
	r := &ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: rec}
	if err := reconcileName(t, r.Reconcile, "app", "web"); err != nil {
		t.Fatal(err)
	}
	if len(rec.reasons) != 1 || rec.reasons[0] != ReasonServiceHasNoSelector || rec.types[0] != corev1.EventTypeWarning {
		t.Fatalf("want one Warning %s, got %v %v", ReasonServiceHasNoSelector, rec.types, rec.reasons)
	}
}
