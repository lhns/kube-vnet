// Package podresolution implements the admission-time half of pod
// resolution (ADR 0034).
//
// The operator stamps `kube-vnet.system/net.<homeNS>.<vnet>=<direction>`
// labels onto pods; NetworkPolicy membership selectors match on those
// stamps. Asynchronously — the reconciler's path — there is a window
// between the apiserver persisting a pod and the stamp landing, during
// which the pod is selected by no membership policy and is therefore
// denied. Field-observed at <=1s on kube-router v2.10.0, where a denial
// presents as an immediate RST; long enough to break a client that does
// not retry.
//
// These handlers close that window by resolving inside the apiserver's
// write path, so a pod carries its stamps from the instant it exists.
//
// Both handlers delegate to controller.Resolver — the same code the
// reconciler runs. There is deliberately no admission-time copy of the
// resolution logic: the two disagreeing would be indistinguishable from a
// policy bug.
package podresolution

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

// Mutator stamps resolved membership labels onto pods during admission.
//
// Its webhook configuration uses `failurePolicy: Ignore` deliberately: this
// handler is an acceleration, not a gate. If it is unreachable or errors,
// admission proceeds, the pod lands unstamped, the deny-all baseline keeps
// it unreachable rather than over-permissive, and the reconciler stamps it
// moments later exactly as it does today. The Validator is what enforces
// correctness, and it fails closed.
type Mutator struct {
	Resolver *controller.Resolver
	Reader   client.Reader
	NSFilter *controller.NamespaceFilter
	Decoder  admission.Decoder
}

func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// The namespace is the authority on whether we touch this pod at all.
	// The webhook configuration also carries a namespaceSelector, but that
	// cannot express --disabled-namespaces, so the check is repeated here.
	managed, err := namespaceManaged(ctx, m.Reader, m.NSFilter, req.Namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if !managed {
		return admission.Allowed("namespace is not managed by kube-vnet")
	}

	desired, _, err := m.Resolver.DesiredLabels(ctx, pod)
	if err != nil {
		// failurePolicy: Ignore turns this into "admit unstamped", which is
		// the pre-webhook behaviour rather than an outage.
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Only labels a previous resolution put there may be pruned. A managed
	// label the REQUEST introduces is forgery, and silently dropping it would
	// admit the pod with no explanation — the ValidatingAdmissionPolicy this
	// replaces denied outright, and users should keep that message. Leaving
	// it in place hands the decision to the Validator, which denies it.
	oldManaged := map[string]string{}
	if len(req.OldObject.Raw) > 0 {
		oldPod := &corev1.Pod{}
		if err := m.Decoder.DecodeRaw(req.OldObject, oldPod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		oldManaged = managedLabels(oldPod)
	}

	out := pod.DeepCopy()
	applyDesiredPreservingUnknown(out, desired, oldManaged)
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	// Pods have no meaningful Generation, so this annotation is a presence
	// marker: membership generation skips pods that lack it (fail-closed
	// during the window this webhook exists to eliminate).
	out.Annotations[controller.AnnotationResolvedGeneration] = "0"
	// Diagnostic only — lets `kubectl get pod -o yaml` show which path
	// stamped, which matters when the webhook is unreachable and the
	// reconciler silently takes over.
	out.Annotations[controller.AnnotationResolvedBy] = controller.ResolvedByAdmission

	marshaled, err := json.Marshal(out)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// namespaceManaged reads the Namespace from cache and applies the same
// filter the reconcilers use.
func namespaceManaged(ctx context.Context, reader client.Reader, filter *controller.NamespaceFilter, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read namespace %q: %w", name, err)
	}
	return filter.IsManaged(ns), nil
}

// applyDesiredPreservingUnknown writes the resolved labels onto pod and
// removes managed labels that a previous resolution had set but that
// resolution no longer produces — the case where a user drops a join label and
// its stamp has to go with it.
//
// It deliberately does NOT touch a managed label this request introduced.
// The reconciler prunes every undesired managed label, which is right there:
// it is authoritative and has no requester to answer to. It is wrong here. At
// admission an unexplained stamp is somebody trying to forge membership, and
// that should be refused with a message rather than quietly deleted.
func applyDesiredPreservingUnknown(pod *corev1.Pod, desired, oldManaged map[string]string) {
	if len(desired) > 0 && pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	for k, v := range desired {
		pod.Labels[k] = v
	}
	for k := range managedLabels(pod) {
		if _, want := desired[k]; want {
			continue
		}
		if _, wasResolved := oldManaged[k]; wasResolved {
			delete(pod.Labels, k)
		}
	}
}
