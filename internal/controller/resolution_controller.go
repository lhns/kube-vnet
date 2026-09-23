package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// `kube-vnet.system/net.<VnetKey>=<direction>`. Generator selectors match on
// these (not on the user-input `kube-vnet/net.<vnet>` labels). See ADR 0030.
const LabelSystemNetPrefix = "kube-vnet.system/net."

// AnnotationResolvedGeneration is the marker the resolution controller writes
// once a pod has been resolved. The generator uses it to skip pods that
// haven't been resolved yet (fail-closed during the race window).
const AnnotationResolvedGeneration = "kube-vnet.system/resolved-generation"

// AnnotationResolvedBy records which path stamped the pod: the admission
// webhook (ADR 0034) or the reconciler. Purely diagnostic — nothing branches
// on it — but when the webhook is unreachable the reconciler takes over
// silently, and this is the only way to see that from the pod itself.
const AnnotationResolvedBy = "kube-vnet.system/resolved-by"

const (
	ResolvedByAdmission  = "admission"
	ResolvedByController = "controller"
)

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

// resolver returns the shared Resolver over this reconciler's client. The
// reconciler writes; the Resolver only reads, so handing it the same client
// as a client.Reader is safe and keeps one implementation of resolution.
func (r *ResolutionReconciler) resolver() *Resolver {
	return &Resolver{Reader: r.Client, NSFilter: r.NSFilter, Recorder: r.Recorder}
}

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

	// Resolve through the shared Resolver — the same code path the
	// admission webhook runs, so a pod stamped at admission reconciles to
	// an identical label set and this pass is a no-op (ADR 0034).
	desired, res, err := r.resolver().DesiredLabels(ctx, pod)
	if err != nil {
		logger.Error(err, "build resolution layers")
		return ctrl.Result{}, err
	}

	// Stamp the result onto the pod.
	if err := r.applyResolution(ctx, pod, desired); err != nil {
		logger.Error(err, "apply resolution")
		return ctrl.Result{}, err
	}
	r.warnNarrowedMembership(pod, res)

	return ctrl.Result{}, nil
}

// warnNarrowedMembership emits a Warning on the pod for each conflict and
// rejected override. Only the reconciler emits: it runs for every pod, the
// webhook included, and admission is not a place to write to the apiserver.
func (r *ResolutionReconciler) warnNarrowedMembership(pod *corev1.Pod, res ResolutionResult) {
	if r.Recorder == nil {
		return
	}
	for _, c := range res.Conflicts {
		parts := make([]string, 0, len(c.Participants))
		for _, p := range c.Participants {
			parts = append(parts, fmt.Sprintf("%s=%s", p.Source, p.Direction))
		}
		sort.Strings(parts)
		r.Recorder.Eventf(pod, nil, corev1.EventTypeWarning, ReasonResolutionConflict, "Resolve",
			"rules in the %s tier disagree on %q (%s) and intersect to %q%s",
			c.Scope, c.Vnet, strings.Join(parts, ", "), c.Effective, notAMember(c.Effective))
	}
	for _, o := range res.OverrideRejected {
		r.Recorder.Eventf(pod, nil, corev1.EventTypeWarning, ReasonOverrideRejected, "Resolve",
			"the %s tier tried to set %q to %q, but the %s tier pins it to %q; "+
				"use a default-* value there to allow overrides",
			o.AttemptedScope, o.Vnet, o.AttemptedDir, o.BlockingScope, o.BlockingDir)
	}
}

func notAMember(d Direction) string {
	if d.Bare() == DirectionNone {
		return ": the pod is not a member"
	}
	return ""
}

// ReasonVirtualNetworkNotJoinable is the Event reason emitted when a
// baseline/binding/label rule names a vnet the pod's namespace cannot join.
// It is deliberately uniform across system and user vnets (ADR 0043): the
// reason is the machine contract that alerts and field-selectors key on, so
// it must never branch on vnet kind. Only the human-readable note is
// enriched, by notJoinableHint.
const ReasonVirtualNetworkNotJoinable = "VirtualNetworkNotJoinable"

// ReasonInvalidJoinLabelDirection is the Event reason emitted on a Pod when one
// of its `kube-vnet/net.*` join labels carries a direction value the operator
// doesn't recognize (anything other than both, ingress, egress, none). It is
// the operator-side counterpart to the admission VAP: the same bad value is
// surfaced at reconcile time even on clusters where the VAP isn't installed
// (Kubernetes < 1.30, or when disabled), so a typo never fails silently. The
// vnet-owner-facing mirror is the vnet's `UnknownDirection`/`InvalidJoiners`
// condition, which fires only when the named vnet exists.
const ReasonInvalidJoinLabelDirection = "InvalidJoinLabelDirection"

// Event reasons for the two ways resolution silently narrows a pod's
// membership (ADR 0031). Both results are correct and fail closed; without
// these the only symptom is a pod that is not a member, with nothing saying
// why.
const (
	// ReasonResolutionConflict: rules in the same tier gave different
	// directions for one vnet and were intersected.
	ReasonResolutionConflict = "ResolutionConflict"
	// ReasonOverrideRejected: a lower tier tried to change a direction an
	// upper tier pinned with a bare value.
	ReasonOverrideRejected = "OverrideRejected"
)

// notJoinableHint returns a targeted suggestion when a ref names one of the
// reserved system vnets, whose namespace semantics trip people up. It is
// formatting only and must never influence control flow (ADR 0043).
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
// and the reserved system-vnet names are legitimately bare.
func bareJoinLabelHint(labelKey, suffix string) string {
	if strings.Contains(suffix, ".") ||
		suffix == SystemVnetCluster || suffix == SystemVnetNamespace {
		return ""
	}
	return fmt.Sprintf(" hint: the bare form %q is only honored in the vnet's home namespace; "+
		"to join a vnet hosted in another namespace use the prefixed form %q.",
		labelKey, fmt.Sprintf("%snet.<homeNS>.%s", DefaultLabelPrefix, suffix))
}

// CanonicalSuffix translates a label suffix (the part after `kube-vnet/net.`
// or `kube-vnet.system/net.`) into the canonical form per ADR 0033, with the
// cluster-singleton exception per ADR 0033 (Amendment):
//
//   - cluster (bare or prefixed `<X>.cluster`) → `cluster`
//     The reserved-name VAP forbids user-authored vnets named `cluster`, so
//     any `<anything>.cluster` is the cluster singleton and collapses to bare.
//   - prefixed `<homeNS>.<name>`  → `<homeNS>.<name>` (already FQ, pass-through)
//   - bare `namespace`            → `<scopeNS>.namespace`
//   - bare user vnet `<name>`     → `<scopeNS>.<name>`
//
// `scopeNS` is the pod's namespace.
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

// applyResolution patches the pod's operator-managed labels to desired and
// marks it resolved. No write happens when both are already in place.
func (r *ResolutionReconciler) applyResolution(ctx context.Context, pod *corev1.Pod, desired map[string]string) error {
	patched := pod.DeepCopy()
	labelsChanged := syncManagedLabels(patched, IsResolutionManagedLabel, desired)
	if !labelsChanged && pod.Annotations[AnnotationResolvedGeneration] != "" {
		return nil
	}
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[AnnotationResolvedGeneration] = fmt.Sprintf("%d", pod.Generation)
	// Set on the patch path only; writing it unconditionally would make every
	// reconcile of a webhook-stamped pod an API write.
	patched.Annotations[AnnotationResolvedBy] = ResolvedByController
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

// desiredHostPortStamps returns the `kube-vnet.system/host-port.<port>.<proto>`
// keys for every hostPort the pod declares (ADR 0040), for the
// HostPortReconciler to select on. Uses the same key set as that reconciler,
// so hostNetwork pods get none.
func desiredHostPortStamps(pod *corev1.Pod) map[string]bool {
	out := map[string]bool{}
	for key := range desiredHostPortKeys([]corev1.Pod{*pod}) {
		out[LabelSystemHostPortPrefix+key.String()] = true
	}
	return out
}

// stripStampedLabels removes the operator-managed labels and the resolution
// annotations from a pod in a disabled namespace.
func (r *ResolutionReconciler) stripStampedLabels(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	patched := pod.DeepCopy()
	// Empty desired-set → syncManagedLabels removes every managed label.
	labelsChanged := syncManagedLabels(patched, IsResolutionManagedLabel, nil)
	_, hasGen := pod.Annotations[AnnotationResolvedGeneration]
	_, hasBy := pod.Annotations[AnnotationResolvedBy]
	if !labelsChanged && !hasGen && !hasBy {
		return ctrl.Result{}, nil
	}
	delete(patched.Annotations, AnnotationResolvedGeneration)
	delete(patched.Annotations, AnnotationResolvedBy)
	if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ResolutionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Pod status updates must not trigger resolution.
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
		// Resolution reads two namespace properties: the `kube-vnet/disabled`
		// annotation and the labels `allowedNamespaces.selector` matches on.
		// See ADR 0044.
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToPods),
			builder.WithPredicates(predicate.Or(
				predicate.AnnotationChangedPredicate{},
				predicate.LabelChangedPredicate{},
			)),
		).
		// Vnet existence and allowedNamespaces are resolution inputs (see
		// vnetToAffectedPods). GenerationChangedPredicate keeps the vnet
		// controller's status writes from fanning out to pods; Create and
		// Delete still fire. See ADR 0030.
		Watches(
			&vnetv1alpha1.VirtualNetwork{},
			handler.EnqueueRequestsFromMapFunc(r.vnetToAffectedPods),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Complete(r)
}

// podsIn returns reconcile requests for the pods in the given namespaces; an
// empty name means cluster-wide.
func (r *ResolutionReconciler) podsIn(ctx context.Context, namespaces ...string) []reconcile.Request {
	seen := map[types.NamespacedName]bool{}
	var out []reconcile.Request
	for _, ns := range namespaces {
		var pods corev1.PodList
		var opts []client.ListOption
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		// A partial fan-out beats none; the next event retries.
		if err := r.List(ctx, &pods, opts...); err != nil {
			continue
		}
		for i := range pods.Items {
			nn := types.NamespacedName{Namespace: pods.Items[i].Namespace, Name: pods.Items[i].Name}
			if seen[nn] {
				continue
			}
			seen[nn] = true
			out = append(out, reconcile.Request{NamespacedName: nn})
		}
	}
	return out
}

// namespaceToPods fans a Namespace event to every pod in it.
func (r *ResolutionReconciler) namespaceToPods(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.podsIn(ctx, obj.GetName())
}

// clusterBaselineToPods fans a ClusterVirtualNetworkBaseline event to every pod
// cluster-wide. The baseline cascades to all managed namespaces so every pod
// re-resolves; coarse but the singleton baseline rarely changes.
func (r *ResolutionReconciler) clusterBaselineToPods(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.podsIn(ctx, "")
}

// namespaceBaselineToPods fans a VirtualNetworkBaseline event to every pod in
// the baseline's namespace.
func (r *ResolutionReconciler) namespaceBaselineToPods(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.podsIn(ctx, obj.GetNamespace())
}

// vnbToPods maps a VirtualNetworkBinding event to all pods in the binding's
// namespace — a binding only ever selects pods there.
func (r *ResolutionReconciler) vnbToPods(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.podsIn(ctx, obj.GetNamespace())
}

// vnetToAffectedPods maps a VirtualNetwork event to the pods it could change.
//
// Vnet existence and allowedNamespaces are inputs to resolution: a rule
// naming a not-yet-created vnet resolves to no stamp, and since the pod
// predicate is change-based nothing else would revisit it.
//
// The affected set is the pods in the namespaces the vnet admits, which covers
// every membership source without matching each one. On update the handler
// maps both revisions, so narrowing allowedNamespaces also reaches the pods it
// excluded. See ADR 0044.
func (r *ResolutionReconciler) vnetToAffectedPods(ctx context.Context, obj client.Object) []reconcile.Request {
	vnet, ok := obj.(*vnetv1alpha1.VirtualNetwork)
	if !ok || vnet == nil {
		return nil
	}
	admitted, err := NamespacesAdmittedBy(ctx, r.Client, vnet)
	if err != nil {
		return nil
	}
	return r.podsIn(ctx, admitted...)
}
