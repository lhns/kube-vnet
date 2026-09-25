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
// cluster state. It is the only implementation of pod resolution: the
// ResolutionReconciler and both admission webhooks (ADR 0034) share it, so
// they cannot drift (podresolution's parity_test.go locks them together).
// It only reads, so Reader may be a cache.
type Resolver struct {
	Reader client.Reader
	// Recorder surfaces VirtualNetworkNotJoinable and InvalidDirection
	// Warnings; nil disables them. The webhook passes nil: admission is no
	// place to write, and the reconciler emits the same Events moments later.
	Recorder events.EventRecorder
}

// DesiredLabels returns the complete set of operator-managed labels the pod
// should carry: membership stamps (ADR 0033) and host-port stamps (ADR 0040).
// It is the desired set, not a diff: SyncStamps also prunes what is no longer
// desired, since a stale membership stamp is a stale grant. The
// ResolutionResult carries the diagnostics (conflicts, rejected overrides).
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
	tiers := []struct {
		scope ResolutionScope
		rules func() ([]ResolutionRule, error)
	}{
		{ScopeClusterBaseline, func() ([]ResolutionRule, error) { return r.clusterBaselineRules(ctx, pod) }},
		{ScopeNamespaceBaseline, func() ([]ResolutionRule, error) { return r.namespaceBaselineRules(ctx, pod) }},
		// Bindings and pod labels share the pod tier, intersecting on conflict.
		{ScopePod, func() ([]ResolutionRule, error) {
			rules, err := r.bindingRules(ctx, pod)
			if err != nil {
				return nil, err
			}
			return append(rules, r.podLabelRules(pod)...), nil
		}},
	}
	var layers []ResolutionLayer
	for _, tier := range tiers {
		rules, err := tier.rules()
		if err != nil {
			return nil, err
		}
		// Only vnets the pod's namespace may join are stamped (ADR 0043).
		if rules, err = r.filterPermittedRules(ctx, rules, pod.Namespace); err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			layers = append(layers, ResolutionLayer{Scope: tier.scope, Rules: rules})
		}
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
		return fmt.Sprintf("%q is not a valid VirtualNetwork reference", key)
	}
	var v vnetv1alpha1.VirtualNetwork
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: homeNS, Name: name}, &v); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("VirtualNetwork %s does not exist", key.Display())
		}
		return fmt.Sprintf("VirtualNetwork %s could not be read to tell why (%v)", key.Display(), err)
	}
	return fmt.Sprintf("VirtualNetwork %s does not permit namespace %q; its owner can add it to spec.allowedNamespaces",
		key.Display(), podNS)
}

// filterPermittedRules drops rules naming vnets the pod's namespace may not
// join (Permits) and emits a VirtualNetworkNotJoinable Warning on the object
// that declared each, so a wrong `virtualNetworkRef.namespace` is visible
// (ADR 0043). A transient error propagates instead, so the caller requeues
// rather than stripping a possibly valid stamp.
func (r *Resolver) filterPermittedRules(ctx context.Context, rules []ResolutionRule, podNS string) ([]ResolutionRule, error) {
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
					"cannot join VirtualNetwork %s (from %s): %s.%s%s",
					rule.Vnet.Display(), rule.Source,
					r.notJoinableNote(ctx, rule.Vnet, podNS), notJoinableHint(rule.Ref), rule.Hint)
			}
			continue
		}
		// Permission is decided on the fully-qualified key so a wrong
		// `<ns>.cluster` ref can be denied; identity is stamped in ADR 0033's
		// canonical form, where the cluster singleton is bare `cluster`.
		// Only survivors reach here, so the collapse is safe.
		rule.Vnet = stampedVnetKey(rule.Vnet)
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
		return nil, client.IgnoreNotFound(err)
	}
	// Owned by the pod, not the baseline: an Event on a cluster-scoped object
	// lands in `default`, where it would name this namespace to anyone who
	// can read `default`.
	return baselineRules(cb.Spec.Memberships, pod.Namespace, "ClusterVirtualNetworkBaseline/default", pod), nil
}

// namespaceBaselineRules reads the singleton VirtualNetworkBaseline named
// `default` in the pod's namespace. Same NotFound-vs-transient split as
// clusterBaselineRules.
func (r *Resolver) namespaceBaselineRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	nb := &vnetv1alpha1.VirtualNetworkBaseline{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: "default"}, nb); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return baselineRules(nb.Spec.Memberships, pod.Namespace, "VirtualNetworkBaseline/"+pod.Namespace+"/default", nb), nil
}

// baselineRules turns a baseline's memberships into rules, skipping invalid
// directions.
func baselineRules(ms []vnetv1alpha1.BaselineMembership, podNS, source string, owner client.Object) []ResolutionRule {
	out := make([]ResolutionRule, 0, len(ms))
	for _, m := range ms {
		if dir, ok := ParseDirection(m.Direction); ok {
			out = append(out, ResolutionRule{
				Vnet:      canonicalVnetKey(m.VirtualNetworkRef, podNS),
				Direction: dir,
				Source:    source,
				Ref:       m.VirtualNetworkRef,
				Owner:     owner,
			})
		}
	}
	return out
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
	var out []ResolutionRule
	for k, v := range pod.Labels {
		suffix, ok := strings.CutPrefix(k, userJoinPrefix)
		if !ok {
			continue
		}
		dir, ok := ParseBareDirection(v)
		if !ok {
			// Surface the ignored label on the pod, for clusters without the
			// direction-value VAP (Kubernetes < 1.30, or disabled).
			if r.Recorder != nil {
				r.Recorder.Eventf(pod, nil, corev1.EventTypeWarning,
					ReasonInvalidDirection, "Resolve",
					"join label %s has the value %q, which is not a direction; use both, ingress, egress "+
						"or none. The label is ignored until fixed.",
					k, v)
			}
			continue
		}
		if isPrefixedClusterSuffix(suffix) {
			// The cluster vnet is a cluster-wide singleton: a namespace in its
			// join label names nothing, so the label is invalid rather than
			// silently collapsed (ADR 0033, 2026-09-25 amendment).
			if r.Recorder != nil {
				r.Recorder.Eventf(pod, nil, corev1.EventTypeWarning,
					ReasonVirtualNetworkNotJoinable, "Resolve",
					"pod label %s: the cluster network has no namespace; use %s%s. The label is ignored until fixed.",
					k, userJoinPrefix, SystemVnetCluster)
			}
			continue
		}
		out = append(out, ResolutionRule{
			Vnet:      VnetKey(CanonicalSuffix(suffix, pod.Namespace)),
			Direction: dir,
			Source:    "pod label " + k,
			// No Ref: a join label carries no namespace field to be wrong
			// about. The Event still lands on the pod that asked for the
			// unjoinable vnet.
			Owner: pod,
			Hint:  bareJoinLabelHint(k, suffix),
		})
	}
	return out
}

// isPrefixedClusterSuffix reports whether a join-label suffix is the invalid
// namespaced form `<X>.cluster` of the cluster singleton. Only the bare
// `kube-vnet/net.cluster` joins it; vnet names contain no dots and `cluster`
// is reserved, so the form cannot name any other vnet.
func isPrefixedClusterSuffix(suffix string) bool {
	return strings.HasSuffix(suffix, "."+SystemVnetCluster)
}

// stampedVnetKey maps a permitted rule's key to the identity stamped on the
// pod (ADR 0033): the cluster singleton is bare `cluster` even when a
// virtualNetworkRef named its home namespace (ADR 0043); every other key is
// already canonical.
func stampedVnetKey(k VnetKey) VnetKey {
	if _, name, ok := splitVnetKey(k); ok && name == SystemVnetCluster {
		return VnetKey(SystemVnetCluster)
	}
	return k
}

// canonicalVnetKey turns a vnet reference into the VnetKey to check
// permission against. A set `ref.Namespace` is used verbatim; an omitted one
// is the pod's namespace, except for `cluster`, whose key is bare `cluster`
// (ADR 0033 Amendment). It never validates or special-cases a vnet kind (ADR
// 0043): a wrong namespace names a vnet the pod cannot join and is denied by
// filterPermittedRules. A qualified `<ns>.cluster` stays qualified so Permits
// checks the real CR; it collapses to bare only after permission passes.
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
