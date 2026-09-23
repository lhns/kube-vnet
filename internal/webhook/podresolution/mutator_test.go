package podresolution

import (
	"context"
	"encoding/json"
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
	nsFilter := controller.NewNamespaceFilter(nil)
	m := &Mutator{
		Resolver:         &controller.Resolver{Reader: c, NSFilter: nsFilter},
		Reader:           c,
		NSFilter:         nsFilter,
		Decoder:          admission.NewDecoder(testScheme(t)),
		OperatorUsername: operatorUser,
	}

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
