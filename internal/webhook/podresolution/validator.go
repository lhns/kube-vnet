package podresolution

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

// Validator enforces that the `kube-vnet.system/*` labels on a pod are the
// ones resolution produces.
//
// It replaces the pods rule of the system-labels ValidatingAdmissionPolicy.
// The Mutator's patch is attributed to the requesting user, not the operator,
// so the VAP's username exemption would reject every pod the Mutator stamps.
// Unlike CEL, this handler can recompute resolution, so instead of "only the
// operator may touch these labels" it checks that a stamp matches resolution.
// It runs with `failurePolicy: Fail`, so forgery stays impossible while the
// operator is down.
//
// Only the delta is policed; enforcing the full set would deny an unrelated
// edit on a pod whose stamps are mid-reconcile. Both rules accept the
// Mutator's output:
//
//  1. a managed label the request adds or changes must equal the resolved
//     value;
//  2. a managed label the request removes must no longer be resolved, which
//     still lets the Mutator prune a stamp whose join label the same request
//     removed.
//
// Annotations are not policed, matching the VAP: a forged
// `resolved-generation` grants nothing without a stamp.
type Validator struct {
	Resolver *controller.Resolver
	Reader   client.Reader
	NSFilter *controller.NamespaceFilter
	Decoder  admission.Decoder
	// OperatorUsername is the operator ServiceAccount's username, exempt
	// from these checks so the reconciler's own patches go through.
	OperatorUsername string
}

func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if v.OperatorUsername != "" && req.UserInfo.Username == v.OperatorUsername {
		return admission.Allowed("operator ServiceAccount")
	}

	pod := &corev1.Pod{}
	if err := v.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	oldManaged := map[string]string{}
	if len(req.OldObject.Raw) > 0 {
		oldPod := &corev1.Pod{}
		if err := v.Decoder.DecodeRaw(req.OldObject, oldPod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		oldManaged = managedLabels(oldPod)
	}
	newManaged := managedLabels(pod)

	// Checked first so the common case skips the namespace lookup and the
	// resolve.
	if maps.Equal(oldManaged, newManaged) {
		return admission.Allowed("no change to kube-vnet.system labels")
	}

	desired := map[string]string{}
	managed, err := namespaceManaged(ctx, v.Reader, v.NSFilter, req.Namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if managed {
		desired, _, err = v.Resolver.DesiredLabels(ctx, pod)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}
	// An unmanaged namespace resolves to nothing, so any stamp added there is
	// rejected below.

	var bad []string
	for k, val := range newManaged {
		if oldManaged[k] == val {
			continue // untouched by this request
		}
		want, ok := desired[k]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s=%s (pod resolves to no such membership)", k, val))
		} else if want != val {
			bad = append(bad, fmt.Sprintf("%s=%s (resolves to %q)", k, val, want))
		}
	}
	for k := range oldManaged {
		if _, still := newManaged[k]; still {
			continue
		}
		if want, ok := desired[k]; ok {
			bad = append(bad, fmt.Sprintf("%s removed (still resolves to %q)", k, want))
		}
	}

	if len(bad) > 0 {
		sort.Strings(bad)
		return admission.Denied(fmt.Sprintf(
			"labels under kube-vnet.system/ are managed by the kube-vnet operator and must match "+
				"the resolved membership for this pod: %s. To declare pod membership in a virtual "+
				"network, use a label under kube-vnet/ instead (e.g. kube-vnet/net.<vnet>=<direction>).",
			strings.Join(bad, "; ")))
	}
	return admission.Allowed("kube-vnet.system labels match resolved membership")
}

func managedLabels(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	for k, v := range pod.Labels {
		if controller.IsResolutionManagedLabel(k) {
			out[k] = v
		}
	}
	return out
}
