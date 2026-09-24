package podresolution

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

const operatorUser = "system:serviceaccount:kube-vnet-system:kube-vnet-controller"

func validate(t *testing.T, v *Validator, user string, oldPod, newPod *corev1.Pod) admission.Response {
	t.Helper()
	return v.Handle(context.Background(), admissionRequest(t, user, oldPod, newPod))
}

// admissionRequest is user's CREATE of newPod, or an UPDATE when oldPod is set.
func admissionRequest(t *testing.T, user string, oldPod, newPod *corev1.Pod) admission.Request {
	t.Helper()
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: newPod.Namespace,
		UserInfo:  authenticationv1.UserInfo{Username: user},
		Object:    runtime.RawExtension{Raw: mustJSON(t, newPod)},
	}}
	if oldPod != nil {
		req.Operation = admissionv1.Update
		req.OldObject = runtime.RawExtension{Raw: mustJSON(t, oldPod)}
	}
	return req
}

func mustJSON(t *testing.T, o any) []byte {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func withLabels(p *corev1.Pod, kv map[string]string) *corev1.Pod {
	out := p.DeepCopy()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	for k, v := range kv {
		out.Labels[k] = v
	}
	return out
}

func TestValidator(t *testing.T) {
	const stamp = "kube-vnet.system/net.app.web"
	web := []client.Object{vnet("web", "app", nil)}
	joined := pod("app", map[string]string{"kube-vnet/net.web": "both"})
	stamped := withLabels(joined, map[string]string{stamp: "both"})
	stale := pod("app", map[string]string{"kube-vnet.system/net.app.gone": "both"})
	staleAnnotated := stale.DeepCopy()
	staleAnnotated.Annotations = map[string]string{"example.com/note": "hi"}

	for _, tc := range []struct {
		name     string
		objects  []client.Object
		user     string // default alice
		disabled []string
		old, new *corev1.Pod // old nil: CREATE
		allowed  bool
		msg      string // substring of the denial
	}{
		// A stamp the pod does not resolve to would make it a member of a vnet
		// nobody granted it; CEL cannot resolve, so the VAP couldn't catch it.
		{name: "forged stamp", objects: web, new: withLabels(pod("app", nil), map[string]string{stamp: "both"}), msg: stamp},
		// An added empty value must not pass as "unchanged" because a missing
		// key also reads as "".
		{name: "forged empty stamp", objects: web, new: withLabels(pod("app", nil), map[string]string{stamp: ""})},
		// The correct value is not forgery; admitting it lets the mutator's
		// output through.
		{name: "correct stamp", objects: web, new: stamped, allowed: true},
		// Stripping a stamp the pod still resolves to silently drops it out of
		// a vnet whose policies still name it.
		{name: "stripping a valid stamp", objects: web, old: stamped, new: joined, msg: "removed"},
		// Why removal can't simply be forbidden: the mutator prunes a stamp
		// together with its join label.
		{name: "pruning a stamp with its join label", objects: web, old: stamped, new: pod("app", map[string]string{}), allowed: true},
		// Only the delta is policed: a stale stamp the request doesn't touch is
		// the reconciler's to fix, not a reason to deny an unrelated edit.
		{name: "untouched stale stamp", old: stale, new: staleAnnotated, allowed: true},
		// The mutator fails open, so an unstamped pod must be admitted; the
		// reconciler stamps it later.
		{name: "unstamped pod", objects: web, new: joined, allowed: true},
		// The operator writes these labels.
		{name: "operator exempt", user: operatorUser, new: withLabels(pod("app", nil), map[string]string{stamp: "both"}), allowed: true},
		// An unmanaged namespace resolves to nothing, so any stamp there is
		// unresolved.
		{name: "stamp in an unmanaged namespace", disabled: []string{"kube-system"},
			new: withLabels(pod("kube-system", nil), map[string]string{"kube-vnet.system/net.kube-system.web": "both"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &Validator{newDeps(t, newClient(t, tc.objects, tc.new), tc.disabled...)}
			resp := validate(t, v, cmp.Or(tc.user, "alice"), tc.old, tc.new)
			if resp.Allowed != tc.allowed {
				t.Fatalf("allowed = %v, want %v: %+v", resp.Allowed, tc.allowed, resp.Result)
			}
			if tc.msg != "" && !strings.Contains(resp.Result.Message, tc.msg) {
				t.Errorf("denial %q should mention %q", resp.Result.Message, tc.msg)
			}
		})
	}
}

// The mutator writes system labels as the requesting user, and the validator
// sees them. If the two disagree, every pod creation in a managed namespace
// fails admission; this is why the system-labels VAP could not stay.
func TestValidator_AcceptsMutatorOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects []client.Object
		pod     *corev1.Pod
	}{
		{"pod label", []client.Object{vnet("web", "app", nil)},
			pod("app", map[string]string{"kube-vnet/net.web": "both"})},
		{"no membership", nil, pod("app", nil)},
		{"hostPort", nil, hostPortPod("app", false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Resolve exactly as the mutator does, then hand the result to
			// the validator the way the apiserver would.
			c := newClient(t, tc.objects, tc.pod)
			r := &controller.Resolver{Reader: c}
			desired, _, err := r.DesiredLabels(context.Background(), tc.pod)
			if err != nil {
				t.Fatalf("DesiredLabels: %v", err)
			}
			mutated := tc.pod.DeepCopy()
			controller.SyncStamps(mutated, desired, nil)

			if resp := validate(t, &Validator{newDeps(t, c)}, "alice", nil, mutated); !resp.Allowed {
				t.Fatalf("the validator rejected the mutator's own output: %+v\n"+
					"this would block every pod creation in a managed namespace",
					resp.Result)
			}
		})
	}
}
