package podresolution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

// The reconciler's own patch must pass through untouched. Mutating it would
// rewrite resolved-by to "admission" on a pod the reconciler stamped, so the
// annotation could no longer show that the webhook missed the pod.
func TestMutator_OperatorPatchIsNotMutated(t *testing.T) {
	p := pod("app", map[string]string{"kube-vnet/net.web": "both"})
	stamped := withLabels(p, map[string]string{"kube-vnet.system/net.app.web": "both"})
	stamped.Annotations = map[string]string{
		controller.AnnotationResolvedGeneration: "1",
		controller.AnnotationResolvedBy:         controller.ResolvedByController,
	}
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)
	m := &Mutator{newDeps(t, c)}

	handle := func(user string) admission.Response {
		return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: "app",
			UserInfo:  authenticationv1.UserInfo{Username: user},
			Object:    runtime.RawExtension{Raw: mustJSON(t, stamped)},
			OldObject: runtime.RawExtension{Raw: mustJSON(t, p)},
		}})
	}

	if resp := handle(operatorUser); !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("operator patch was mutated: allowed=%v patches=%s", resp.Allowed, mustJSON(t, resp.Patches))
	}

	// Control: the same request from anyone else is mutated, so the check
	// above is not passing for an unrelated reason.
	resp := handle("alice")
	if !resp.Allowed {
		t.Fatalf("user request denied: %+v", resp.Result)
	}
	ops, _ := json.Marshal(resp.Patches)
	if len(resp.Patches) == 0 {
		t.Fatalf("control request was not mutated; the test cannot tell the exemption apart: %s", ops)
	}
}

// A stamp the request supplies is the request's claim, not the mutator's to
// rewrite. A wrong value for a vnet the pod really is in must reach the
// validator and be denied - the same outcome as the admission policy without
// the webhook - instead of being silently corrected.
func TestMutator_LeavesRequestStampsForTheValidator(t *testing.T) {
	stamp := "kube-vnet.system/net.app.web"
	for _, tc := range []struct {
		name      string
		value     string
		wantAdmit bool
	}{
		{"wrong value is denied", "egress", false},
		{"resolved value passes", "both", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pod("app", map[string]string{"kube-vnet/net.web": "both", stamp: tc.value})
			c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)
			d := newDeps(t, c)

			mutated := mutate(t, &Mutator{d}, "alice", nil, p)
			if got := mutated.Labels[stamp]; got != tc.value {
				t.Fatalf("the mutator rewrote the request's stamp %q to %q", tc.value, got)
			}
			resp := validate(t, &Validator{d}, "alice", nil, mutated)
			if resp.Allowed != tc.wantAdmit {
				t.Fatalf("validator allowed=%v, want %v: %+v", resp.Allowed, tc.wantAdmit, resp.Result)
			}
			if !tc.wantAdmit && !strings.Contains(resp.Result.Message, `resolves to "both"`) {
				t.Errorf("denial should say what the stamp resolves to, got: %s", resp.Result.Message)
			}
		})
	}
}

// Stamps the request did not touch are still the mutator's: it prunes one
// whose join label the same request removed.
func TestMutator_PrunesUntouchedStaleStamp(t *testing.T) {
	stamp := "kube-vnet.system/net.app.web"
	old := pod("app", map[string]string{"kube-vnet/net.web": "both", stamp: "both"})
	updated := pod("app", map[string]string{stamp: "both"}) // join label dropped, stamp untouched
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, updated)

	if got := mutate(t, &Mutator{newDeps(t, c)}, "alice", old, updated); got.Labels[stamp] != "" {
		t.Fatalf("stale stamp survived: %v", got.Labels)
	}
}
