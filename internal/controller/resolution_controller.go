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
	Scheme      *runtime.Scheme
	NSFilter    *NamespaceFilter
	LabelPrefix string
	// OperatorNamespace is the chart's release namespace, where the cluster
	// system vnet lives. Used to canonicalize the cluster-vnet membership
	// label key on every pod that inherits the cluster baseline (per ADR 0033).
	OperatorNamespace string
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

	// 1. Cluster baseline: the ClusterVirtualNetworkBaseline singleton named
	// `default`.
	if rules := r.clusterBaselineRules(ctx, pod); len(rules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopeClusterBaseline, Rules: rules})
	}

	// 2. Namespace baseline (ScopeNamespaceBaseline).
	if nsBaselineRules := r.namespaceBaselineRules(ctx, pod); len(nsBaselineRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopeNamespaceBaseline, Rules: nsBaselineRules})
	}

	// 3. Pod tier (ScopePod): VirtualNetworkBindings + pod labels merged into
	// a single layer. Within-layer intersection applies on conflict.
	var podRules []ResolutionRule
	podRules = append(podRules, r.bindingRules(ctx, pod)...)
	podRules = append(podRules, r.podLabelRules(pod)...)
	if len(podRules) > 0 {
		layers = append(layers, ResolutionLayer{Scope: ScopePod, Rules: podRules})
	}

	return layers, nil
}

// clusterBaselineRules reads the singleton ClusterVirtualNetworkBaseline
// named `default` (if it exists). Absent → no rules.
func (r *ResolutionReconciler) clusterBaselineRules(ctx context.Context, pod *corev1.Pod) []ResolutionRule {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default"}, cb); err != nil {
		return nil
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
		})
	}
	return out
}

// namespaceBaselineRules reads the singleton VirtualNetworkBaseline named
// `default` in the pod's namespace.
func (r *ResolutionReconciler) namespaceBaselineRules(ctx context.Context, pod *corev1.Pod) []ResolutionRule {
	nb := &vnetv1alpha1.VirtualNetworkBaseline{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: "default"}, nb); err != nil {
		return nil
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
		})
	}
	return out
}

// bindingRules reads VirtualNetworkBindings in the pod's namespace that
// match the pod's labels. Pod-tier source.
func (r *ResolutionReconciler) bindingRules(ctx context.Context, pod *corev1.Pod) []ResolutionRule {
	var vnbs vnetv1alpha1.VirtualNetworkBindingList
	if err := r.List(ctx, &vnbs, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}
	var out []ResolutionRule
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
		dir, ok := ParseBareDirection(dirStr)
		if !ok {
			continue
		}
		out = append(out, ResolutionRule{
			Vnet:      r.canonicalVnetKey(b.Spec.VirtualNetworkRef, pod.Namespace),
			Direction: dir,
			Source:    "VirtualNetworkBinding/" + b.Name,
		})
	}
	return out
}

// podLabelRules reads the pod's own kube-vnet/net.<suffix>=<direction>
// labels. Pod-tier source. Per ADR 0033, the suffix can be either bare
// (`<vnet>` — only valid for the vnet's home NS or for system vnets) or
// prefixed (`<homeNS>.<vnet>`); both forms canonicalize to the same FQ
// VnetKey.
func (r *ResolutionReconciler) podLabelRules(pod *corev1.Pod) []ResolutionRule {
	prefix := r.LabelPrefix
	if prefix == "" {
		prefix = DefaultLabelPrefix
	}
	userNetPrefix := prefix + "net."
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
		})
	}
	return out
}

// canonicalKeyFromPodLabelSuffix translates a pod-label suffix (the part
// after `kube-vnet/net.`) into the canonical FQ VnetKey. Thin wrapper that
// supplies the receiver's OperatorNamespace; logic lives in the free
// CanonicalSuffix function so the baseline generator can reuse the same rule.
func (r *ResolutionReconciler) canonicalKeyFromPodLabelSuffix(suffix, podNS string) VnetKey {
	return VnetKey(CanonicalSuffix(suffix, podNS, r.OperatorNamespace))
}

// CanonicalSuffix translates a label suffix (the part after `kube-vnet/net.`
// or `kube-vnet.system/net.`) into the canonical FQ form per ADR 0033.
// The same rule applies to pod-input labels (resolution controller) and to
// `--elide-baseline-for` entries (baseline generator):
//
//   - prefixed `<homeNS>.<name>`  → `<homeNS>.<name>` (already FQ, pass-through)
//   - bare `cluster`              → `<operatorNS>.cluster` (the lone exception:
//                                   cluster vnet's home is the operator NS,
//                                   not the rendering scope)
//   - bare `namespace`            → `<scopeNS>.namespace`
//   - bare user vnet `<name>`     → `<scopeNS>.<name>`
//
// `scopeNS` is the pod's NS (resolution) or the baseline's NS (elide list).
// If `operatorNS` is empty (test/out-of-cluster fallback), bare `cluster`
// stays bare so the policy generator can still match.
func CanonicalSuffix(suffix, scopeNS, operatorNS string) string {
	if strings.IndexByte(suffix, '.') >= 0 {
		return suffix
	}
	if suffix == SystemVnetCluster {
		if operatorNS == "" {
			return SystemVnetCluster
		}
		return operatorNS + "." + SystemVnetCluster
	}
	return scopeNS + "." + suffix
}

// canonicalVnetKey computes the canonical FQ VnetKey for any vnet
// reference, given the pod's namespace as the resolution context. Per
// ADR 0033, the output is always `<homeNS>.<name>` — system vnets included
// — where `homeNS` is the namespace the vnet lives in (the pod's NS for
// the per-NS `namespace` system vnet; the operator's release NS for
// `cluster`; the ref's own namespace for user vnets).
func (r *ResolutionReconciler) canonicalVnetKey(ref vnetv1alpha1.VirtualNetworkRef, podNS string) VnetKey {
	if ref.Name == SystemVnetNamespace {
		return VnetKey(podNS + "." + SystemVnetNamespace)
	}
	if ref.Name == SystemVnetCluster {
		opNS := r.OperatorNamespace
		if opNS == "" {
			opNS = ref.Namespace
		}
		return VnetKey(opNS + "." + SystemVnetCluster)
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
		Complete(r)
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
