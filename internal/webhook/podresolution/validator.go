package podresolution

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

// Validator enforces that the `kube-vnet.system/*` labels on a pod are the
// ones resolution actually produces.
//
// It replaces the pods rule of the system-labels ValidatingAdmissionPolicy,
// which could not survive the Mutator. A mutating webhook's patch is
// attributed to the *original requester*, never to the operator's
// ServiceAccount, so the VAP's username exemption does not cover it: with
// the Mutator running, that policy would reject every pod creation in a
// managed namespace. (ADR 0034's "VAP exemption" section assumed otherwise
// and is wrong.)
//
// CEL cannot recompute resolution, so the VAP could only enforce "nobody
// but the operator may touch these labels". This handler can recompute, so
// it enforces the stronger property directly — a forged stamp that
// disagrees with resolution is rejected, and one that agrees is by
// definition the correct value. It runs with `failurePolicy: Fail`, keeping
// the policy's posture that forgery is impossible even while the operator
// is unreachable.
//
// Only the *delta* is policed. A label the request did not touch is the
// reconciler's problem, not the requester's: enforcing the full set would
// deny an innocent `kubectl annotate` on a pod whose stamps happen to be
// mid-reconcile. Two rules, both satisfied by the Mutator's own output:
//
//  1. a managed label this request adds or changes must equal the resolved
//     value — blocks forging membership;
//  2. a managed label this request removes must no longer be resolved —
//     blocks stripping a still-valid stamp, while still allowing the
//     Mutator to prune a stamp whose join label the same request removed.
//
// Annotations are deliberately not policed, matching the VAP. Forging
// `resolved-generation` alone buys nothing: membership also requires a
// stamp, and stamps can no longer be forged.
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

	// Nothing to police. Checked before the namespace lookup and the
	// resolve so the overwhelmingly common case — a pod nobody is trying
	// to forge stamps on — costs one map scan.
	if equalLabels(oldManaged, newManaged) {
		return admission.Allowed("no change to kube-vnet.system labels")
	}

	desired := map[string]string{}
	managed, err := v.namespaceManaged(ctx, req.Namespace)
	if err != nil {
		// failurePolicy: Fail — an unreachable operator blocks the write
		// rather than letting an unverifiable stamp through.
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if managed {
		desired, _, err = v.Resolver.DesiredLabels(ctx, pod)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}
	// An unmanaged namespace resolves to no stamps at all, so any label
	// added there is by definition unresolved and gets rejected below.

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

func (v *Validator) namespaceManaged(ctx context.Context, name string) (bool, error) {
	m := &Mutator{Reader: v.Reader, NSFilter: v.NSFilter}
	return m.namespaceManaged(ctx, name)
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

func equalLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
