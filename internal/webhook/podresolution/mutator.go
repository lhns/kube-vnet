package podresolution

import (
	"context"
	"encoding/json"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
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
		resp := admission.Allowed("namespace is not managed by kube-vnet")
		if req.Operation == admissionv1.Create {
			// The pod gets no wait here, nor any NetworkPolicy from kube-vnet.
			if w := controller.NetworkWaitWarning(r.pod, false, false); w != "" {
				resp = resp.WithWarnings(w)
			}
		}
		return resp
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

	// Init containers can only be set at creation.
	var warning string
	if req.Operation == admissionv1.Create {
		var wait *corev1.Container
		if wait, warning = networkWait(out, m.NetworkWait); wait != nil {
			// First, so the kubelet publishes the pod IP while it runs and
			// nothing else starts before the rules are live.
			out.Spec.InitContainers = append([]corev1.Container{*wait}, out.Spec.InitContainers...)
		}
	}

	marshaled, err := json.Marshal(out)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	resp := admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
	if warning != "" {
		resp = resp.WithWarnings(warning)
	}
	return resp
}
