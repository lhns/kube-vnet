package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// LabelSystemNetPrefix is the prefix on operator-stamped membership labels:
// `kube-vnet.system/net.<vnet>=<direction>`. Generator selectors match on
// these (not on the user-input `kube-vnet/net.<vnet>` labels). See ADR 0030.
const LabelSystemNetPrefix = "kube-vnet.system/net."

// AnnotationResolvedGeneration is the marker the resolution controller writes
// once a pod has been resolved. The generator uses it to skip pods that
// haven't been resolved yet (fail-closed during the race window).
const AnnotationResolvedGeneration = "kube-vnet.system/resolved-generation"

// ResolutionReconciler resolves the inheritance lattice for each pod and
// stamps `kube-vnet.system/net.<vnet>=<direction>` labels accordingly. Three
// scopes per ADR 0031:
//   - ScopeClusterBaseline: the ClusterVirtualNetworkBaseline named `default`.
//   - ScopeNamespaceBaseline: the VirtualNetworkBaseline named `default` in
//     the pod's namespace (if present).
//   - ScopePod: VirtualNetworkBindings matching the pod, plus the pod's own
//     `kube-vnet/net.<vnet>=<direction>` labels. All sources within this
//     scope intersect on conflict (fail-closed).
//
// On change to any of those input sources, the affected pod(s) get
// re-resolved. Disabled namespaces are skipped entirely.
type ResolutionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
	// Recorder surfaces VirtualNetworkNotJoinable Warning Events on the
	// object that declared an unjoinable rule. Optional; nil disables the
	// diagnostic (unit tests construct the reconciler without one).
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=clustervirtualnetworkbaselines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=clustervirtualnetworkbaselines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbaselines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbaselines/status,verbs=get;update;patch

func (r *ResolutionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Skip pods being deleted; their labels don't matter.
	if pod.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Skip pods in disabled namespaces — operator stays out entirely.
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !r.NSFilter.IsManaged(ns) {
		return r.stripStampedLabels(ctx, pod)
	}

	// Build the four resolution layers from current cluster state.
	layers, err := r.buildLayers(ctx, pod, ns)
	if err != nil {
		logger.Error(err, "build resolution layers")
		return ctrl.Result{}, err
	}
	res := Resolve(layers)

	// Stamp the result onto the pod.
	if err := r.applyResolution(ctx, pod, res); err != nil {
		logger.Error(err, "apply resolution")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ResolutionReconciler) buildLayers(ctx context.Context, pod *corev1.Pod, ns *corev1.Namespace) ([]ResolutionLayer, error) {
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

// ReasonVirtualNetworkNotJoinable is the Event reason emitted when a
// baseline/binding/label rule names a vnet the pod's namespace cannot join.
// It is deliberately uniform across system and user vnets (ADR 0043): the
// reason is the machine contract that alerts and field-selectors key on, so
// it must never branch on vnet kind. Only the human-readable note is
// enriched, by notJoinableHint.
const ReasonVirtualNetworkNotJoinable = "VirtualNetworkNotJoinable"

// notJoinableHint returns a targeted suggestion when a ref names one of the
// reserved system vnets, whose namespace semantics trip people up. It is
// PURE FORMATTING — it must never influence control flow, or the per-kind
// special-casing ADR 0043 removed would creep back in.
func notJoinableHint(ref vnetv1alpha1.VirtualNetworkRef) string {
	switch ref.Name {
	case SystemVnetCluster:
		return " hint: `cluster` is a cluster-wide singleton living in the operator's namespace; " +
			"omit `namespace` (recommended) or set it to the operator's namespace."
	case SystemVnetNamespace:
		return " hint: the `namespace` system vnet exists in every managed namespace — not in the " +
			"operator's (unmanaged) namespace; omit `namespace` to mean the pod's own namespace."
	default:
		return ""
	}
}

// bareJoinLabelHint returns the guidance to append when a *bare* pod join label
// `kube-vnet/net.<X>` can't be honored: the bare form is only resolved against
// the pod's own namespace, so a missing local vnet usually means the user meant
// a vnet hosted elsewhere and should use the prefixed form. suffix is the label
// key's tail (the part after `kube-vnet/net.`); a dot means it's already the
// prefixed `<homeNS>.<name>` form (fully covered by notJoinableNote — no hint),
// and the reserved system-vnet names are legitimately bare. Folded in from the
// retired JoinLabelDiagnosticReconciler (ADR 0027).
func bareJoinLabelHint(labelKey, suffix string) string {
	if strings.Contains(suffix, ".") ||
		suffix == SystemVnetCluster || suffix == SystemVnetNamespace {
		return ""
	}
	return fmt.Sprintf(" hint: the bare form %q is only honored in the vnet's home namespace; "+
		"to join a vnet hosted in another namespace use the prefixed form %q.",
		labelKey, fmt.Sprintf("%snet.<homeNS>.%s", DefaultLabelPrefix, suffix))
}

// notJoinableNote explains WHY the pod cannot join, distinguishing "no such
// vnet" from "exists but doesn't allow you" — different problems with
// different fixes. Only called on the failure path, so the extra Get is free
// in the happy case.
func (r *ResolutionReconciler) notJoinableNote(ctx context.Context, key VnetKey, podNS string) string {
	homeNS, name, ok := splitVnetKey(key)
	if !ok {
		return fmt.Sprintf("malformed virtual network key %q", key)
	}
	var v vnetv1alpha1.VirtualNetwork
	if err := r.Get(ctx, client.ObjectKey{Namespace: homeNS, Name: name}, &v); err != nil {
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
func (r *ResolutionReconciler) filterPermittedRules(ctx context.Context, rules []ResolutionRule, podNS string) ([]ResolutionRule, error) {
	if len(rules) == 0 {
		return rules, nil
	}
	out := rules[:0]
	for _, rule := range rules {
		ok, err := Permits(ctx, r.Client, rule.Vnet, podNS)
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
func (r *ResolutionReconciler) clusterBaselineRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default"}, cb); err != nil {
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
			Vnet:      r.canonicalVnetKey(m.VirtualNetworkRef, pod.Namespace),
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
func (r *ResolutionReconciler) namespaceBaselineRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	nb := &vnetv1alpha1.VirtualNetworkBaseline{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: "default"}, nb); err != nil {
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
			Vnet:      r.canonicalVnetKey(m.VirtualNetworkRef, pod.Namespace),
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
func (r *ResolutionReconciler) bindingRules(ctx context.Context, pod *corev1.Pod) ([]ResolutionRule, error) {
	var vnbs vnetv1alpha1.VirtualNetworkBindingList
	if err := r.List(ctx, &vnbs, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}
	var out []ResolutionRule
	for i := range vnbs.Items {
		b := &vnbs.Items[i]
		podSel, err := selectorFromLabelSelector(&b.Spec.PodSelector)
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
			Vnet:      r.canonicalVnetKey(b.Spec.VirtualNetworkRef, pod.Namespace),
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
func (r *ResolutionReconciler) podLabelRules(pod *corev1.Pod) []ResolutionRule {
	userNetPrefix := DefaultLabelPrefix + "net."
	var out []ResolutionRule
	for k, v := range pod.Labels {
		if !strings.HasPrefix(k, userNetPrefix) {
			continue
		}
		dir, ok := ParseBareDirection(v)
		if !ok {
			continue
		}
		suffix := strings.TrimPrefix(k, userNetPrefix)
		key := r.canonicalKeyFromPodLabelSuffix(suffix, pod.Namespace)
		out = append(out, ResolutionRule{
			Vnet:      key,
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

// canonicalKeyFromPodLabelSuffix translates a pod-label suffix (the part
// after `kube-vnet/net.`) into the canonical FQ VnetKey via CanonicalSuffix.
func (r *ResolutionReconciler) canonicalKeyFromPodLabelSuffix(suffix, podNS string) VnetKey {
	return VnetKey(CanonicalSuffix(suffix, podNS))
}

// CanonicalSuffix translates a label suffix (the part after `kube-vnet/net.`
// or `kube-vnet.system/net.`) into the canonical form per ADR 0033, with the
// cluster-singleton exception per ADR 0033 (Amendment):
//
//   - cluster (bare or prefixed `<X>.cluster`) → `cluster`
//     The cluster vnet is THE cluster-wide singleton; the prefix is
//     informationless. The reserved-name VAP forbids user-authored vnets
//     named `cluster`, so any `<anything>.cluster` is unambiguously the
//     cluster system vnet and collapses to bare. This inverts the rule
//     for every other vnet.
//   - prefixed `<homeNS>.<name>`  → `<homeNS>.<name>` (already FQ, pass-through)
//   - bare `namespace`            → `<scopeNS>.namespace`
//   - bare user vnet `<name>`     → `<scopeNS>.<name>`
//
// `scopeNS` is the pod's NS for the resolution controller. (Previously also
// used by the baseline generator's elide-list translation; that mechanism
// was removed in ADR 0035.)
func CanonicalSuffix(suffix, scopeNS string) string {
	if suffix == SystemVnetCluster ||
		strings.HasSuffix(suffix, "."+SystemVnetCluster) {
		return SystemVnetCluster
	}
	if strings.IndexByte(suffix, '.') >= 0 {
		return suffix
	}
	return scopeNS + "." + suffix
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
func (r *ResolutionReconciler) canonicalVnetKey(ref vnetv1alpha1.VirtualNetworkRef, podNS string) VnetKey {
	if ref.Namespace == "" {
		if ref.Name == SystemVnetCluster {
			return VnetKey(SystemVnetCluster)
		}
		return VnetKey(podNS + "." + ref.Name)
	}
	return VnetKey(ref.Namespace + "." + ref.Name)
}

func isSystemVnetName(name string) bool {
	return name == SystemVnetNamespace || name == SystemVnetCluster
}

func selectorFromLabelSelector(s *metav1.LabelSelector) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(s)
}

// applyResolution computes the desired kube-vnet.system/net.* +
// kube-vnet.system/host-port.* label set, diffs it against the pod's
// current labels, and patches if needed.
//
// Host-port stamps (ADR 0040): for every container port that declares
// `hostPort != 0`, the resolution controller stamps
// `kube-vnet.system/host-port.<port>.<proto>=true` on the pod. The
// HostPortReconciler then emits a NetworkPolicy whose podSelector matches
// the stamp — making the pod reachable externally on that hostPort.
// Skipped for hostNetwork pods because NetworkPolicy enforcement on them
// is CNI-dependent.
func (r *ResolutionReconciler) applyResolution(ctx context.Context, pod *corev1.Pod, res ResolutionResult) error {
	// Build desired label map: vnet membership labels + host-port stamps.
	desired := map[string]string{}
	for vnet, dir := range res.Effective {
		desired[LabelSystemNetPrefix+string(vnet)] = string(dir)
	}
	for stamp := range desiredHostPortStamps(pod) {
		desired[stamp] = "true"
	}

	// Diff + apply via the shared label-sync helper. Covers both the
	// kube-vnet.system/net.* membership family and the new
	// kube-vnet.system/host-port.* exposure family (ADR 0040).
	patched := pod.DeepCopy()
	labelsChanged := syncManagedLabels(patched, isResolutionManagedLabel, desired)
	if !labelsChanged && pod.Annotations[AnnotationResolvedGeneration] != "" {
		// Already in sync and the resolved-generation annotation is set —
		// no API write needed.
		return nil
	}
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[AnnotationResolvedGeneration] = fmt.Sprintf("%d", pod.Generation)
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

// isResolutionManagedLabel returns true for the two label families the
// resolution controller stamps on pods.
func isResolutionManagedLabel(k string) bool {
	return strings.HasPrefix(k, LabelSystemNetPrefix) ||
		strings.HasPrefix(k, LabelSystemHostPortPrefix)
}

// desiredHostPortStamps returns the set of host-port label keys this pod
// should carry (kube-vnet.system/host-port.<port>.<proto>=true for every
// declared (port, protocol)). Empty for hostNetwork pods.
func desiredHostPortStamps(pod *corev1.Pod) map[string]bool {
	out := map[string]bool{}
	if pod.Spec.HostNetwork {
		return out
	}
	for _, c := range pod.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.HostPort == 0 {
				continue
			}
			proto := cp.Protocol
			if proto == "" {
				proto = corev1.ProtocolTCP
			}
			stamp := LabelSystemHostPortPrefix + fmt.Sprintf("%d.%s", cp.HostPort, strings.ToLower(string(proto)))
			out[stamp] = true
		}
	}
	return out
}

// stripStampedLabels removes any kube-vnet.system/net.* labels (and the
// resolved-generation annotation) from pods in disabled namespaces or pods
// whose namespace transitioned to disabled.
func (r *ResolutionReconciler) stripStampedLabels(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	patched := pod.DeepCopy()
	// Empty desired-set → syncManagedLabels removes every managed label.
	labelsChanged := syncManagedLabels(patched, isResolutionManagedLabel, nil)
	if !labelsChanged && pod.Annotations[AnnotationResolvedGeneration] == "" {
		return ctrl.Result{}, nil
	}
	delete(patched.Annotations, AnnotationResolvedGeneration)
	if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ResolutionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Pod predicate: only react to label or annotation changes (creation also
	// flows through Update events from the cache). This keeps reconcile
	// volume bounded — pod status updates don't trigger us.
	podPredicate := predicate.Or(
		predicate.LabelChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		predicate.GenerationChangedPredicate{},
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named("resolution").
		For(&corev1.Pod{}, builder.WithPredicates(podPredicate)).
		Watches(
			&vnetv1alpha1.ClusterVirtualNetworkBaseline{},
			handler.EnqueueRequestsFromMapFunc(r.clusterBaselineToPods),
		).
		Watches(
			&vnetv1alpha1.VirtualNetworkBaseline{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceBaselineToPods),
		).
		Watches(
			&vnetv1alpha1.VirtualNetworkBinding{},
			handler.EnqueueRequestsFromMapFunc(r.vnbToPods),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToPods),
			builder.WithPredicates(predicate.AnnotationChangedPredicate{}),
		).
		Complete(r)
}

// namespaceToPods fans a Namespace event to every pod in it. Catches
// the `kube-vnet/disabled` annotation flip: on disable, each pod's
// reconcile takes the stripStampedLabels path (the NS fails IsManaged);
// on re-enable, pods get re-resolved and re-stamped. Without this
// watch, a disabled namespace's pods kept their kube-vnet.system/net.*
// stamps — and therefore their vnet memberships — until an unrelated
// pod event or the informer resync. Filtered to annotation changes;
// namespace create is uninteresting (no pods yet) and label-only
// changes don't affect the managed/disabled decision.
func (r *ResolutionReconciler) namespaceToPods(ctx context.Context, obj client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(obj.GetName())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: pods.Items[i].Namespace, Name: pods.Items[i].Name},
		})
	}
	return out
}

// clusterBaselineToPods fans a ClusterVirtualNetworkBaseline event to every
// pod cluster-wide. The baseline cascades to all managed namespaces so
// every pod re-resolves; coarse but the singleton baseline rarely changes.
func (r *ResolutionReconciler) clusterBaselineToPods(ctx context.Context, _ client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: pods.Items[i].Namespace, Name: pods.Items[i].Name},
		})
	}
	return out
}

// namespaceBaselineToPods fans a VirtualNetworkBaseline event to every pod
// in the baseline's namespace.
func (r *ResolutionReconciler) namespaceBaselineToPods(ctx context.Context, obj client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: pods.Items[i].Namespace, Name: pods.Items[i].Name},
		})
	}
	return out
}

// vnbToPods maps a VirtualNetworkBinding event to all pods in the binding's namespace.
func (r *ResolutionReconciler) vnbToPods(ctx context.Context, obj client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: pods.Items[i].Namespace, Name: pods.Items[i].Name},
		})
	}
	return out
}
