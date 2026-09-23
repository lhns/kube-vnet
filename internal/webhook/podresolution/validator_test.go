package podresolution

import (
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

func newValidator(t *testing.T, c client.Client) *Validator {
	return &Validator{newDeps(t, c)}
}

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

// A stamp the pod does not actually resolve to is forgery: it would make the
// pod a member of a vnet nobody granted it. This is the property the
// ValidatingAdmissionPolicy could not express, because CEL cannot resolve.
func TestValidator_ForgedStamp_Denied(t *testing.T) {
	base := pod("app", nil)
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, base)
	v := newValidator(t, c)

	forged := withLabels(base, map[string]string{"kube-vnet.system/net.app.web": "both"})
	resp := validate(t, v, "alice", nil, forged)

	if resp.Allowed {
		t.Fatal("a forged membership stamp was admitted")
	}
	if msg := resp.Result.Message; !strings.Contains(msg, "kube-vnet.system/net.app.web") {
		t.Errorf("denial message should name the offending label, got: %s", msg)
	}
}

// An added label with an empty value must not pass as "unchanged" just because
// a missing key also reads as "".
func TestValidator_ForgedEmptyStamp_Denied(t *testing.T) {
	base := pod("app", nil)
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, base)
	v := newValidator(t, c)

	forged := withLabels(base, map[string]string{"kube-vnet.system/net.app.web": ""})
	if resp := validate(t, v, "alice", nil, forged); resp.Allowed {
		t.Fatal("a forged empty-valued stamp was admitted")
	}
}

// A stamp that matches what resolution produces is not forgery: it is the
// correct value. Admitting it is what lets the mutator's own output through.
func TestValidator_CorrectStamp_Allowed(t *testing.T) {
	base := pod("app", map[string]string{"kube-vnet/net.web": "both"})
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, base)
	v := newValidator(t, c)

	stamped := withLabels(base, map[string]string{"kube-vnet.system/net.app.web": "both"})
	if resp := validate(t, v, "alice", nil, stamped); !resp.Allowed {
		t.Fatalf("a correctly-resolved stamp was rejected: %+v", resp.Result)
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

			if resp := validate(t, newValidator(t, c), "alice", nil, mutated); !resp.Allowed {
				t.Fatalf("the validator rejected the mutator's own output: %+v\n"+
					"this would block every pod creation in a managed namespace",
					resp.Result)
			}
		})
	}
}

// Stripping a stamp the pod still resolves to is the security-relevant
// direction: it would silently drop the pod out of a vnet whose policies
// still name it.
func TestValidator_StrippingValidStamp_Denied(t *testing.T) {
	base := pod("app", map[string]string{"kube-vnet/net.web": "both"})
	stamped := withLabels(base, map[string]string{"kube-vnet.system/net.app.web": "both"})
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, stamped)
	v := newValidator(t, c)

	resp := validate(t, v, "alice", stamped, base)
	if resp.Allowed {
		t.Fatal("stripping a still-resolved membership stamp was admitted")
	}
	if msg := resp.Result.Message; !strings.Contains(msg, "removed") {
		t.Errorf("denial should say the label was removed, got: %s", msg)
	}
}

// The mirror case, and the reason removal cannot simply be forbidden: when
// the join label goes away the stamp must go with it. The mutator prunes it
// in the same request, and the validator has to accept that.
func TestValidator_PruningStampWithItsJoinLabel_Allowed(t *testing.T) {
	old := pod("app", map[string]string{
		"kube-vnet/net.web":            "both",
		"kube-vnet.system/net.app.web": "both",
	})
	// Both the join label and its stamp removed, as the mutator would.
	updated := pod("app", map[string]string{})
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, updated)

	if resp := validate(t, newValidator(t, c), "alice", old, updated); !resp.Allowed {
		t.Fatalf("pruning a stamp whose join label was removed was rejected: %+v", resp.Result)
	}
}

// Only the delta is policed. A stale stamp the request does not touch is the
// reconciler's problem; denying here would reject an unrelated edit for a
// state the user did not cause and cannot fix.
func TestValidator_UntouchedStaleStamp_Allowed(t *testing.T) {
	stale := pod("app", map[string]string{"kube-vnet.system/net.app.gone": "both"})
	c := newClient(t, nil, stale)

	annotated := stale.DeepCopy()
	annotated.Annotations = map[string]string{"example.com/note": "hi"}

	if resp := validate(t, newValidator(t, c), "alice", stale, annotated); !resp.Allowed {
		t.Fatalf("an unrelated edit was rejected because of a pre-existing stale stamp: %+v",
			resp.Result)
	}
}

// The mutating half runs failurePolicy: Ignore, so a pod can legitimately
// arrive unstamped when the webhook was unreachable. That must degrade to
// today's behaviour (controller stamps it later), not a hard failure.
func TestValidator_UnstampedPod_Allowed(t *testing.T) {
	p := pod("app", map[string]string{"kube-vnet/net.web": "both"})
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)

	if resp := validate(t, newValidator(t, c), "alice", nil, p); !resp.Allowed {
		t.Fatalf("an unstamped pod was rejected; a mutator outage must not block "+
			"pod creation: %+v", resp.Result)
	}
}

// The operator's own patches must go through: it is the component that
// writes these labels.
func TestValidator_OperatorServiceAccount_Exempt(t *testing.T) {
	base := pod("app", nil)
	c := newClient(t, nil, base)
	forged := withLabels(base, map[string]string{"kube-vnet.system/net.app.web": "both"})

	if resp := validate(t, newValidator(t, c), operatorUser, nil, forged); !resp.Allowed {
		t.Fatalf("the operator ServiceAccount was not exempt: %+v", resp.Result)
	}
}

// An unmanaged namespace resolves to no memberships at all, so any stamp
// added there is unresolved by construction.
func TestValidator_StampInUnmanagedNamespace_Denied(t *testing.T) {
	base := pod("kube-system", nil)
	c := newClient(t, nil, base)
	v := &Validator{newDeps(t, c, "kube-system")}

	forged := withLabels(base, map[string]string{"kube-vnet.system/net.kube-system.web": "both"})
	if resp := validate(t, v, "alice", nil, forged); resp.Allowed {
		t.Fatal("a stamp was admitted in an unmanaged namespace")
	}
}
