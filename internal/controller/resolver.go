package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Resolver computes the operator-managed labels for a pod from current
// cluster state. It is the single implementation of pod resolution, shared
// by every caller that needs it:
//
//   - ResolutionReconciler, asynchronously after the pod is persisted.
//   - The mutating admission webhook, synchronously during pod admission,
//     so a pod is stamped before it ever runs (ADR 0034).
//   - The validating admission webhook, to check that the system labels on
//     an incoming pod are the ones resolution would produce.
//
// There is deliberately no second implementation: an admission-time copy
// of this logic would drift from the reconciler's, and the two disagreeing
// is indistinguishable from a policy bug. The differential test in
// resolver_parity_test.go locks the paths together.
//
// Every method only reads, so Reader may be a cache-backed client.Reader.
type Resolver struct {
	Reader   client.Reader
	NSFilter *NamespaceFilter
	// Recorder surfaces VirtualNetworkNotJoinable and
	// InvalidJoinLabelDirection Warning Events. Optional; nil disables the
	// diagnostic. The webhook passes nil — admission is not a place to
	// write to the apiserver, and the reconciler emits the same Events
	// moments later on its own pass.
	Recorder events.EventRecorder
}

// DesiredLabels returns the complete set of operator-managed labels the pod
// should carry: `kube-vnet.system/net.<homeNS>.<vnet>=<direction>` membership
// stamps (ADR 0033) plus `kube-vnet.system/host-port.<port>.<proto>=true`
// exposure stamps (ADR 0040).
//
// The returned map is the *desired* set, not a diff. Callers apply it with
// syncManagedLabels, which also prunes managed labels that are no longer
// desired — pruning is part of the contract, not an optimization: a stale
// membership stamp is a stale grant.
//
// The ResolutionResult is returned alongside for callers that need the
// diagnostics (conflicts, rejected overrides); the labels alone are enough
// to decide membership.
func (r *Resolver) DesiredLabels(ctx context.Context, pod *corev1.Pod) (map[string]string, ResolutionResult, error) {
	layers, err := r.buildLayers(ctx, pod)
	if err != nil {
		return nil, ResolutionResult{}, err
	}
	res := Resolve(layers)

	desired := map[string]string{}
	for vnet, dir := range res.Effective {
		desired[LabelSystemNetPrefix+string(vnet)] = string(dir)
	}
	for stamp := range desiredHostPortStamps(pod) {
		desired[stamp] = "true"
	}
	return desired, res, nil
}

func (r *Resolver) buildLayers(ctx context.Context, pod *corev1.Pod) ([]ResolutionLayer, error) {
	var layers []ResolutionLayer

	// Every rule set is filtered through filterPermittedRules before
	// becoming a layer. Rules that reference vnets the pod's NS can't
	// actually join get dropped here, so the system-label stamping that
	// follows resolution only stamps vnets the pod genuinely belongs to.
	// Without this gate, a pod-label or baseline entry pointing at a
	// non-permitting vnet would still stamp `kube-vnet.system/net.*` on
	// the pod — a lying stamp that doesn't match the membership policy
	// the VirtualNetworkReconciler later generates. Dropped rules emit a
	// VirtualNetworkNotJoinable Warning Event. See ADR 0043.

	// 1. Cluster baseline: the ClusterVirtualNetworkBaseline singleton named
	// `default`.
	clusterRules, err := r.clusterBaselineRules(ctx, pod)
	if err != nil {
		return nil, err
	}
	clusterRules, err = r.filterPermittedRules(ctx, clusterRules, pod.Namespace)
	if err != nil {
		return nil, err
	}
	if len(clusterRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopeClusterBaseline, Rules: clusterRules})
	}

	// 2. Namespace baseline (ScopeNamespaceBaseline).
	nsBaselineRules, err := r.namespaceBaselineRules(ctx, pod)
	if err != nil {
		return nil, err
	}
	nsBaselineRules, err = r.filterPermittedRules(ctx, nsBaselineRules, pod.Namespace)
	if err != nil {
		return nil, err
	}
	if len(nsBaselineRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopeNamespaceBaseline, Rules: nsBaselineRules})
	}

	// 3. Pod tier (ScopePod): VirtualNetworkBindings + pod labels merged into
	// a single layer. Within-layer intersection applies on conflict.
	bindRules, err := r.bindingRules(ctx, pod)
	if err != nil {
		return nil, err
	}
	podRules := append(bindRules, r.podLabelRules(pod)...)
	podRules, err = r.filterPermittedRules(ctx, podRules, pod.Namespace)
	if err != nil {
		return nil, err
	}
	if len(podRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopePod, Rules: podRules})
	}

	return layers, nil
}

// notJoinableNote explains WHY the pod cannot join, distinguishing "no such
// vnet" from "exists but doesn't allow you" — different problems with
// different fixes. Only called on the failure path, so the extra Get is free
// in the happy case.
func (r *Resolver) notJoinableNote(ctx context.Context, key VnetKey, podNS string) string {
	homeNS, name, ok := splitVnetKey(key)
	if !ok {
		return fmt.Sprintf("malformed virtual network key %q", key)
	}
	var v vnetv1alpha1.VirtualNetwork
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: homeNS, Name: name}, &v); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("VirtualNetwork %q does not exist in namespace %q", name, homeNS)
		}
		return fmt.Sprintf("could not read VirtualNetwork %s/%s: %v", homeNS, name, err)
	}
	return fmt.Sprintf("VirtualNetwork %s/%s does not permit namespace %q (spec.allowedNamespaces)",
		homeNS, name, podNS)
}

// filterPermittedRules drops rules that reference vnets the pod's NS
// isn't permitted to join (per Permits, the single-source-of-truth
// helper in permits.go). "Not permitted" — vnet doesn't exist, NS not
// in allowedNamespaces — drops the rule and emits a
// VirtualNetworkNotJoinable Warning Event on the object that declared it,
// so a wrong `virtualNetworkRef.namespace` is visible instead of silent
// (ADR 0043). A transient apiserver
// error is NOT the same thing: it propagates as an error so the caller
// requeues instead of stripping a possibly-valid stamp. Collapsing
// errors into "deny" caused stamp churn (momentary membership loss)
// during apiserver blips, with no requeue to recover.
//
// This is the membership gate for the stamping pipeline. The
// VirtualNetworkReconciler does the same check independently when
// generating membership policies; this filter keeps the pod's stamped
// labels honest by deciding the same thing here.
func (r *Resolver) filterPermittedRules(ctx context.Context, rules []ResolutionRule, podNS string) ([]ResolutionRule, error) {
	if len(rules) == 0 {
		return rules, nil
	}
	out := rules[:0]
	for _, rule := range rules {
		ok, err := Permits(ctx, r.Reader, rule.Vnet, podNS)
		if err != nil {
			return nil, err
		}
		if !ok {
			if r.Recorder != nil && rule.Owner != nil {
				r.Recorder.Eventf(rule.Owner, nil, corev1.EventTypeWarning,
					ReasonVirtualNetworkNotJoinable, "Resolve",
					"pod namespace %q cannot join %q (from %s): %s%s%s",
					podNS, rule.Vnet, rule.Source,
					r.notJoinableNote(ctx, rule.Vnet, podNS), notJoinableHint(rule.Ref), rule.Hint)
			}
			continue
		}
		// Permission is decided on the fully-qualified key so a wrong
		// `<ns>.cluster` can be denied; identity is stamped in ADR 0033's
		// canonical form, which collapses `<anything>.cluster` to bare
		// `cluster`. Only survivors reach here, so the collapse is safe.
		rule.Vnet = VnetKey(CanonicalSuffix(string(rule.Vnet), podNS))
		out = append(out, rule)
	}
	return out, nil
}

// clusterBaselineRules reads the singleton ClusterVirtualNetworkBaseline
// named `default` (if it exists). Absent → no rules, nil error. A
// transient Get error propagates — treating it as "no baseline" would
// strip baseline-driven stamps from every pod reconciled during an
// apiserver blip.
func (r *Resolver) clusterBaselineRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: "default"}, cb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ResolutionRule, 0, len(cb.Spec.Memberships))
	for _, m := range cb.Spec.Memberships {
		dir, ok := ParseDirection(m.Direction)
		if !ok {
			continue
		}
		out = append(out, ResolutionRule{
			Vnet:      canonicalVnetKey(m.VirtualNetworkRef, pod.Namespace),
			Direction: dir,
			Source:    "ClusterVirtualNetworkBaseline/default",
			Ref:       m.VirtualNetworkRef,
			Owner:     cb,
		})
	}
	return out, nil
}

// namespaceBaselineRules reads the singleton VirtualNetworkBaseline named
// `default` in the pod's namespace. Same NotFound-vs-transient split as
// clusterBaselineRules.
func (r *Resolver) namespaceBaselineRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	nb := &vnetv1alpha1.VirtualNetworkBaseline{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: "default"}, nb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ResolutionRule, 0, len(nb.Spec.Memberships))
	for _, m := range nb.Spec.Memberships {
		dir, ok := ParseDirection(m.Direction)
		if !ok {
			continue
		}
		out = append(out, ResolutionRule{
			Vnet:      canonicalVnetKey(m.VirtualNetworkRef, pod.Namespace),
			Direction: dir,
			Source:    "VirtualNetworkBaseline/" + pod.Namespace + "/default",
			Ref:       m.VirtualNetworkRef,
			Owner:     nb,
		})
	}
	return out, nil
}

// bindingRules reads VirtualNetworkBindings in the pod's namespace that
// match the pod's labels. Pod-tier source. A List error propagates —
// reading it as "no bindings" would strip binding-driven stamps during
// an apiserver blip.
func (r *Resolver) bindingRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	var vnbs vnetv1alpha1.VirtualNetworkBindingList
	if err := r.Reader.List(ctx, &vnbs, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}
	var out []ResolutionRule
	for i := range vnbs.Items {
		b := &vnbs.Items[i]
		podSel, err := metav1.LabelSelectorAsSelector(&b.Spec.PodSelector)
		if err != nil {
			// Malformed selector on the binding itself: a per-object
			// data problem, not a transient error. Skip the binding;
			// its own reconciler surfaces the condition.
			continue
		}
		if !podSel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		dirStr := b.Spec.Direction
		if dirStr == "" {
			dirStr = string(DirectionBoth)
		}
		dir, ok := ParseBareDirection(dirStr)
		if !ok {
			continue
		}
		out = append(out, ResolutionRule{
			Vnet:      canonicalVnetKey(b.Spec.VirtualNetworkRef, pod.Namespace),
			Direction: dir,
			Source:    "VirtualNetworkBinding/" + b.Name,
			Ref:       b.Spec.VirtualNetworkRef,
			Owner:     b,
		})
	}
	return out, nil
}

// podLabelRules reads the pod's own kube-vnet/net.<suffix>=<direction>
// labels. Pod-tier source. Per ADR 0033, the suffix can be either bare
// (`<vnet>` — only valid for the vnet's home NS or for system vnets) or
// prefixed (`<homeNS>.<vnet>`); both forms canonicalize to the same FQ
// VnetKey.
func (r *Resolver) podLabelRules(pod *corev1.Pod) []ResolutionRule {
	userNetPrefix := DefaultLabelPrefix + "net."
	var out []ResolutionRule
	for k, v := range pod.Labels {
		if !strings.HasPrefix(k, userNetPrefix) {
			continue
		}
		dir, ok := ParseBareDirection(v)
		if !ok {
			// Malformed direction value: membership silently ignores it. Surface
			// it on the pod so the mistake is visible even without the admission
			// VAP (which is absent on Kubernetes < 1.30, or if disabled). This
			// is nearly free — we already parsed the value here, and it only
			// fires for a misconfigured label the user fixes once.
			if r.Recorder != nil {
				r.Recorder.Eventf(pod, nil, corev1.EventTypeWarning,
					ReasonInvalidJoinLabelDirection, "Resolve",
					"join label %q has an unrecognized direction value %q; must be one of "+
						"both, ingress, egress, none (ADR 0030). The label is ignored until fixed.",
					k, v)
			}
			continue
		}
		suffix := strings.TrimPrefix(k, userNetPrefix)
		out = append(out, ResolutionRule{
			Vnet:      VnetKey(CanonicalSuffix(suffix, pod.Namespace)),
			Direction: dir,
			Source:    "<pod-label>",
			// No Ref: a join label carries no namespace field to be wrong
			// about. The Event still lands on the pod that asked for the
			// unjoinable vnet.
			Owner: pod,
			Hint:  bareJoinLabelHint(k, suffix),
		})
	}
	return out
}

// canonicalVnetKey turns a vnet reference into the VnetKey to check
// permission against, using the pod's namespace as the resolution context.
//
// It is pure inference — it never validates and never special-cases a vnet
// kind (ADR 0043). `ref.Namespace` is *honored* whenever it is set; it is
// only inferred when omitted:
//
//   - omitted + `cluster` → bare `cluster`, the singleton's canonical key
//     (ADR 0033 Amendment).
//   - omitted + anything else (the per-NS `namespace` system vnet and user
//     vnets alike) → the pod's own namespace.
//   - set → used verbatim.
//
// A wrong namespace therefore names a vnet the pod cannot join, and is
// denied by the ordinary permission path in filterPermittedRules — exactly
// as a user vnet that doesn't allow the pod would be. It is never rewritten
// to something that happens to work. A *qualified* `<ns>.cluster` key is
// deliberately left qualified so Permits can verify it against the real CR;
// it collapses to the bare canonical form after permission passes.
func canonicalVnetKey(ref vnetv1alpha1.VirtualNetworkRef, podNS string) VnetKey {
	if ref.Namespace == "" {
		if ref.Name == SystemVnetCluster {
			return VnetKey(SystemVnetCluster)
		}
		return VnetKey(podNS + "." + ref.Name)
	}
	return VnetKey(ref.Namespace + "." + ref.Name)
}

// IsResolutionManagedLabel reports whether a label key belongs to one of the
// two families the resolver owns on pods: `kube-vnet.system/net.*` membership
// stamps and `kube-vnet.system/host-port.*` exposure stamps.
func IsResolutionManagedLabel(k string) bool {
	return strings.HasPrefix(k, LabelSystemNetPrefix) ||
		strings.HasPrefix(k, LabelSystemHostPortPrefix)
}

// ServiceAccountUsername renders the apiserver username for a ServiceAccount.
// The validating webhook compares request.userInfo.username against it to
// exempt the operator's own writes, mirroring the check the system-labels
// ValidatingAdmissionPolicy makes for the resources it still covers.
func ServiceAccountUsername(namespace, name string) string {
	if namespace == "" || name == "" {
		return ""
	}
	return "system:serviceaccount:" + namespace + ":" + name
}
