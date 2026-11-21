package podresolution

import (
	"context"
	"strings"
	"testing"

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
		return m.Handle(context.Background(), admissionRequest(t, user, p, stamped))
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
	if len(resp.Patches) == 0 {
		t.Fatal("control request was not mutated; the test cannot tell the exemption apart")
	}
}

// On UPDATE a stamp the request supplies is the request's claim, not the
// mutator's to rewrite. A wrong value for a vnet the pod really is in must
// reach the validator and be denied - the same outcome as the admission policy
// without the webhook - instead of being silently corrected. (On CREATE the
// mutator replaces it; see TestMutator_CreateReplacesStaleStamps.)
func TestMutator_UpdateLeavesRequestStampsForTheValidator(t *testing.T) {
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
			old := pod("app", map[string]string{"kube-vnet/net.web": "both"})
			p := pod("app", map[string]string{"kube-vnet/net.web": "both", stamp: tc.value})
			c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)
			d := newDeps(t, c)

			mutated := mutate(t, &Mutator{d}, "alice", old, p)
			if got := mutated.Labels[stamp]; got != tc.value {
				t.Fatalf("the mutator rewrote the request's stamp %q to %q", tc.value, got)
			}
			resp := validate(t, &Validator{d}, "alice", old, mutated)
			if resp.Allowed != tc.wantAdmit {
				t.Fatalf("validator allowed=%v, want %v: %+v", resp.Allowed, tc.wantAdmit, resp.Result)
			}
			if !tc.wantAdmit && !strings.Contains(resp.Result.Message, `resolves to "both"`) {
				t.Errorf("denial should say what the stamp resolves to, got: %s", resp.Result.Message)
			}
		})
	}
}

// A pod created with stamps (restored by Velero, or copied from
// `kubectl get -o yaml`) gets them recomputed, and the request is told which
// were replaced. Both a stamp for another namespace and a wrong direction go.
func TestMutator_CreateReplacesStaleStamps(t *testing.T) {
	good := "kube-vnet.system/net.app.web"
	foreign := "kube-vnet.system/net.other.web"
	p := pod("app", map[string]string{
		"kube-vnet/net.web": "both",
		good:                "egress", // wrong direction
		foreign:             "both",   // another namespace's stamp
	})
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)
	m := &Mutator{newDeps(t, c)}

	got, resp := mutateWithResponse(t, m, "alice", nil, p)
	if got.Labels[good] != "both" {
		t.Errorf("wrong direction not rewritten: %v", got.Labels)
	}
	if _, ok := got.Labels[foreign]; ok {
		t.Errorf("foreign stamp survived: %v", got.Labels)
	}
	if len(resp.Warnings) != 1 {
		t.Fatalf("want one warning, got %q", resp.Warnings)
	}
	w := resp.Warnings[0]
	if !strings.Contains(w, good+", "+foreign) {
		t.Errorf("warning should name both keys, sorted: %s", w)
	}
	if v := validate(t, &Validator{newDeps(t, c)}, "alice", nil, got); !v.Allowed {
		t.Errorf("validator rejected the mutator's output: %+v", v.Result)
	}

	// Control: correct stamps (or none) warn about nothing.
	clean := pod("app", map[string]string{"kube-vnet/net.web": "both", good: "both"})
	if _, resp := mutateWithResponse(t, m, "alice", nil, clean); len(resp.Warnings) != 0 {
		t.Errorf("unexpected warning for correct stamps: %q", resp.Warnings)
	}
}

// The stale-stamp warning and the network-wait warning are independent.
func TestMutator_CreateStaleStampAndNetworkWaitWarnings(t *testing.T) {
	p := withMaxWait("not-a-duration")
	p.Namespace = "app"
	p.Labels["kube-vnet.system/net.other.web"] = "both"
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)

	_, resp := mutateWithResponse(t, &Mutator{newDeps(t, c)}, "alice", nil, p)
	if len(resp.Warnings) != 2 {
		t.Fatalf("want both warnings, got %q", resp.Warnings)
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
