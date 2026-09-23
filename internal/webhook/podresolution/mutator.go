package podresolution

import (
	"context"
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

// Mutator stamps resolved membership onto pods during admission.
//
// It runs with `failurePolicy: Ignore` because it is an acceleration, not a
// gate: if it fails, the pod is admitted unstamped, the deny-all baseline keeps
// it isolated, and the reconciler stamps it shortly after. The Validator
// enforces correctness and fails closed.
type Mutator struct{ Deps }

func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if m.isOperator(req) {
		return admission.Allowed("operator ServiceAccount")
	}
	r, err := m.decode(req)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	managed, err := m.namespaceManaged(ctx, req.Namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if !managed {
		return admission.Allowed("namespace is not managed by kube-vnet")
	}

	desired, _, err := m.Resolver.DesiredLabels(ctx, r.pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	out := r.pod.DeepCopy()
	// Only stamps the request left untouched are ours to write or prune. A
	// stamp the request supplied stays as submitted, so the Validator rejects
	// a forged value with a message instead of it being silently rewritten -
	// the same outcome as the admission policy without the webhook.
	controller.SyncStamps(out, desired, r.untouched)
	controller.MarkResolved(out, controller.ResolvedByAdmission)

	marshaled, err := json.Marshal(out)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}
