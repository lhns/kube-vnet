package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// OperatorMembership is one entry in the operator-default-memberships list.
// VnetKey is the bare suffix (e.g. "namespace", "cluster") that goes after
// `kube-vnet.system/net.`.
type OperatorMembership struct {
	Vnet      VnetKey
	Direction Direction
}

// ResolutionReconciler resolves the inheritance lattice for each pod and
// stamps `kube-vnet.system/net.<vnet>=<direction>` labels accordingly. Reads:
// operator defaults (struct field), ClusterVirtualNetworkBindings (cluster
// scope), VirtualNetworkBindings (pod's namespace scope), and pod's own
// `kube-vnet/net.<vnet>=<direction>` labels (highest priority).
//
// On change to any of those four input sources, the affected pod(s) get
// re-resolved. Disabled namespaces are skipped entirely (operator stays out).
//
// See ADR 0030.
type ResolutionReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	NSFilter         *NamespaceFilter
	OperatorDefaults []OperatorMembership
	LabelPrefix      string
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;update

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

	// 1. Operator defaults.
	if len(r.OperatorDefaults) > 0 {
		rules := make([]ResolutionRule, 0, len(r.OperatorDefaults))
		for _, d := range r.OperatorDefaults {
			rules = append(rules, ResolutionRule{
				Vnet:      d.Vnet,
				Direction: d.Direction,
				Source:    "operator-default",
			})
		}
		layers = append(layers, ResolutionLayer{Scope: ScopeOperatorDefault, Rules: rules})
	}

	// 2. ClusterVirtualNetworkBindings — match on namespaceSelector + podSelector.
	var cvnbs vnetv1alpha1.ClusterVirtualNetworkBindingList
	if err := r.List(ctx, &cvnbs); err != nil {
		return nil, fmt.Errorf("list cvnbs: %w", err)
	}
	var clusterRules []ResolutionRule
	for i := range cvnbs.Items {
		b := &cvnbs.Items[i]
		nsSel, err := selectorFromLabelSelector(&b.Spec.NamespaceSelector)
		if err != nil {
			continue
		}
		if !nsSel.Matches(labels.Set(ns.Labels)) {
			continue
		}
		podSel, err := selectorFromLabelSelector(&b.Spec.PodSelector)
		if err != nil {
			continue
		}
		if !podSel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		dirStr := b.Spec.Direction
		if dirStr == "" {
			dirStr = string(DirectionBoth)
		}
		dir, ok := ParseDirection(dirStr)
		if !ok {
			continue
		}
		clusterRules = append(clusterRules, ResolutionRule{
			Vnet:      vnetKeyForBinding(b.Spec.VirtualNetworkRef, pod.Namespace),
			Direction: dir,
			Source:    b.Name,
		})
	}
	if len(clusterRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopeClusterBinding, Rules: clusterRules})
	}

	// 3. VirtualNetworkBindings in the pod's namespace — match on podSelector.
	var vnbs vnetv1alpha1.VirtualNetworkBindingList
	if err := r.List(ctx, &vnbs, client.InNamespace(pod.Namespace)); err != nil {
		return nil, fmt.Errorf("list vnbs: %w", err)
	}
	var nsRules []ResolutionRule
	for i := range vnbs.Items {
		b := &vnbs.Items[i]
		podSel, err := selectorFromLabelSelector(&b.Spec.PodSelector)
		if err != nil {
			continue
		}
		if !podSel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		dirStr := b.Spec.Direction
		if dirStr == "" {
			dirStr = string(DirectionBoth)
		}
		dir, ok := ParseDirection(dirStr)
		if !ok {
			continue
		}
		nsRules = append(nsRules, ResolutionRule{
			Vnet:      vnetKeyForBinding(b.Spec.VirtualNetworkRef, pod.Namespace),
			Direction: dir,
			Source:    b.Name,
		})
	}
	if len(nsRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopeNamespaceBinding, Rules: nsRules})
	}

	// 4. Pod-authored labels — `kube-vnet/net.<suffix>=<direction>`.
	prefix := r.LabelPrefix
	if prefix == "" {
		prefix = DefaultLabelPrefix
	}
	userNetPrefix := prefix + "net."
	var podRules []ResolutionRule
	for k, v := range pod.Labels {
		if !strings.HasPrefix(k, userNetPrefix) {
			continue
		}
		dir, ok := ParseDirection(v)
		if !ok {
			// Invalid value; skip (the direction VAP rejects these at admission).
			continue
		}
		suffix := strings.TrimPrefix(k, userNetPrefix)
		podRules = append(podRules, ResolutionRule{
			Vnet:      VnetKey(suffix),
			Direction: dir,
			Source:    "<pod-label>",
		})
	}
	if len(podRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopePodLabel, Rules: podRules})
	}

	return layers, nil
}

// vnetKeyForBinding computes the label suffix the binding's target vnet
// produces from the pod's namespace's perspective. System vnets ("namespace"
// and "cluster") are always bare (never prefixed), even when the binding
// targets a vnet object in a different namespace from the pod's. User vnets
// follow the existing bare/prefixed convention from JoinLabelKey.
func vnetKeyForBinding(ref vnetv1alpha1.VirtualNetworkRef, podNS string) VnetKey {
	if isSystemVnetName(ref.Name) {
		return VnetKey(ref.Name)
	}
	if ref.Namespace == podNS {
		return VnetKey(ref.Name)
	}
	return VnetKey(ref.Namespace + "." + ref.Name)
}

func isSystemVnetName(name string) bool {
	return name == SystemVnetNamespace || name == SystemVnetCluster
}

func selectorFromLabelSelector(s *metav1.LabelSelector) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(s)
}

// applyResolution computes the desired kube-vnet.system/net.* label set,
// diffs it against the pod's current labels, and patches if needed.
func (r *ResolutionReconciler) applyResolution(ctx context.Context, pod *corev1.Pod, res ResolutionResult) error {
	// Build desired label map.
	desired := map[string]string{}
	for vnet, dir := range res.Effective {
		desired[LabelSystemNetPrefix+string(vnet)] = string(dir)
	}

	// Diff against current.
	current := map[string]string{}
	for k, v := range pod.Labels {
		if strings.HasPrefix(k, LabelSystemNetPrefix) {
			current[k] = v
		}
	}
	if mapsEqualSorted(current, desired) {
		// Already in sync — no patch needed (avoid generating unnecessary writes).
		// Still ensure the resolved-generation annotation is up to date.
		gen := pod.Annotations[AnnotationResolvedGeneration]
		if gen != "" {
			return nil
		}
	}

	// Build the patch: merge the desired keys, remove stale ones.
	patched := pod.DeepCopy()
	if patched.Labels == nil {
		patched.Labels = map[string]string{}
	}
	for k := range patched.Labels {
		if strings.HasPrefix(k, LabelSystemNetPrefix) {
			if _, keep := desired[k]; !keep {
				delete(patched.Labels, k)
			}
		}
	}
	for k, v := range desired {
		patched.Labels[k] = v
	}
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[AnnotationResolvedGeneration] = fmt.Sprintf("%d", pod.Generation)

	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

// stripStampedLabels removes any kube-vnet.system/net.* labels (and the
// resolved-generation annotation) from pods in disabled namespaces or pods
// whose namespace transitioned to disabled.
func (r *ResolutionReconciler) stripStampedLabels(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	hasStamped := false
	for k := range pod.Labels {
		if strings.HasPrefix(k, LabelSystemNetPrefix) {
			hasStamped = true
			break
		}
	}
	if !hasStamped && pod.Annotations[AnnotationResolvedGeneration] == "" {
		return ctrl.Result{}, nil
	}
	patched := pod.DeepCopy()
	for k := range patched.Labels {
		if strings.HasPrefix(k, LabelSystemNetPrefix) {
			delete(patched.Labels, k)
		}
	}
	delete(patched.Annotations, AnnotationResolvedGeneration)
	if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func mapsEqualSorted(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			return false
		}
	}
	return true
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
			&vnetv1alpha1.ClusterVirtualNetworkBinding{},
			handler.EnqueueRequestsFromMapFunc(r.cvnbToPods),
		).
		Watches(
			&vnetv1alpha1.VirtualNetworkBinding{},
			handler.EnqueueRequestsFromMapFunc(r.vnbToPods),
		).
		Complete(r)
}

// cvnbToPods maps a ClusterVirtualNetworkBinding event to all pods cluster-wide.
// Coarse — every pod re-resolves on any CVNB change. Acceptable for v1; can
// narrow to selector-matched pods later if bindings churn at high frequency.
func (r *ResolutionReconciler) cvnbToPods(ctx context.Context, _ client.Object) []reconcile.Request {
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
