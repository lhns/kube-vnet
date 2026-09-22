// Package podresolution resolves pod membership at admission (ADR 0034).
//
// Membership policies select pods by the `kube-vnet.system/*` stamps. When
// only the reconciler writes them, a new pod is denied until its stamp lands
// (observed at up to 1s on kube-router, long enough to break clients that do
// not retry). Resolving in the apiserver's write path means a pod carries its
// stamps from the moment it exists.
//
// Both handlers delegate to controller.Resolver, the code the reconciler
// runs, so the two paths cannot disagree.
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
// It runs with `failurePolicy: Ignore` because it is an acceleration, not a
// gate: if it fails, the pod is admitted unstamped, the deny-all baseline keeps
// it isolated, and the reconciler stamps it shortly after. The Validator
// enforces correctness and fails closed.
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

	// The webhook's namespaceSelector cannot express --disabled-namespaces.
	managed, err := namespaceManaged(ctx, m.Reader, m.NSFilter, req.Namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if !managed {
		return admission.Allowed("namespace is not managed by kube-vnet")
	}

	desired, _, err := m.Resolver.DesiredLabels(ctx, pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

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
	// Pods have no meaningful Generation; this is a presence marker, since
	// membership generation skips pods that lack it.
	out.Annotations[controller.AnnotationResolvedGeneration] = "0"
	// Diagnostic: shows whether the webhook or the reconciler stamped the pod.
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
// prunes managed labels that the old object carried but resolution no longer
// produces (a join label was dropped, so its stamp goes too).
//
// Unlike the reconciler, it leaves alone a managed label the request itself
// introduced. That is a forgery attempt, and dropping it silently would admit
// the pod without explanation; left in place, the Validator denies it with a
// message.
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
