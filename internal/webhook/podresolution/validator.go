package podresolution

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator enforces that the `kube-vnet.system/*` labels on a pod are the
// ones resolution produces.
//
// For the pods the webhook sees, it takes over from the system-labels
// ValidatingAdmissionPolicy, which keeps policing everything else (excluded
// namespaces, and pods/status everywhere). The Mutator's patch is attributed
// to the requesting user, not the operator, so the policy's username exemption
// would reject every pod the Mutator stamps. Unlike CEL, this handler can
// recompute resolution, so it checks that a stamp matches resolution. It runs
// with `failurePolicy: Fail`, so forgery stays impossible while the operator
// is down.
//
// Only stamps the request touched are policed; enforcing the full set would
// deny an unrelated edit on a pod whose stamps are mid-reconcile. A touched
// stamp must equal the resolved value, and a removed one must no longer be
// resolved (which still lets the Mutator prune a stamp whose join label the
// same request removed).
//
// Annotations are not policed, matching the policy: a forged
// `resolved-generation` grants nothing without a stamp.
type Validator struct{ Deps }

func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if v.isOperator(req) {
		return admission.Allowed("operator ServiceAccount")
	}
	r, err := v.decode(req)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var touched []string
	for k := range r.stamps {
		if !r.untouched(k) {
			touched = append(touched, k)
		}
	}
	for k := range r.oldStamps {
		if _, still := r.stamps[k]; !still {
			touched = append(touched, k)
		}
	}
	// The common case: nobody is forging stamps. Skip the lookup and resolve.
	if len(touched) == 0 {
		return admission.Allowed("no change to kube-vnet.system labels")
	}

	desired := map[string]string{}
	managed, err := v.namespaceManaged(ctx, req.Namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if managed {
		if desired, _, err = v.Resolver.DesiredLabels(ctx, r.pod); err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}
	// An unmanaged namespace resolves to nothing, so any stamp added there is
	// rejected below.

	var bad []string
	for _, k := range touched {
		val, present := r.stamps[k]
		want, resolved := desired[k]
		switch {
		case present && !resolved:
			bad = append(bad, fmt.Sprintf("%s=%s (pod resolves to no such membership)", k, val))
		case present && want != val:
			bad = append(bad, fmt.Sprintf("%s=%s (resolves to %q)", k, val, want))
		case !present && resolved:
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
