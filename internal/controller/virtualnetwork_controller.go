package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// VirtualNetworkReconciler reconciles VirtualNetwork resources into NetworkPolicies.
type VirtualNetworkReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	LabelPrefix string
	NSFilter    *NamespaceFilter
}

// +kubebuilder:rbac:groups=kube-vnet,resources=virtualnetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet,resources=virtualnetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet,resources=virtualnetworks/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the controller-runtime Reconciler interface.
func (r *VirtualNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("vnet", req.NamespacedName)

	vnet := &vnetv1alpha1.VirtualNetwork{}
	if err := r.Get(ctx, req.NamespacedName, vnet); err != nil {
		if apierrors.IsNotFound(err) {
			// VirtualNetwork is gone — clean up any policies that still carry its label.
			return ctrl.Result{}, r.cleanupForDeleted(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}

	// If the VirtualNetwork is being deleted, drop policies and return.
	if !vnet.DeletionTimestamp.IsZero() {
		if err := r.cleanupForDeleted(ctx, vnet.Namespace, vnet.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Reject home namespace if it is unmanaged — produce no policies and surface a Degraded condition.
	if !r.NSFilter.IsManagedName(vnet.Namespace) {
		setReady(vnet, metav1.ConditionFalse, "HomeNamespaceExcluded",
			fmt.Sprintf("home namespace %q is excluded by the operator", vnet.Namespace))
		setDegraded(vnet, metav1.ConditionTrue, "HomeNamespaceExcluded",
			fmt.Sprintf("home namespace %q is in the operator excluded list", vnet.Namespace))
		return ctrl.Result{}, r.updateStatus(ctx, vnet, nil, nil)
	}

	// Compute desired members from pods cluster-wide (Cluster extent) or in the home NS (Namespace extent).
	members, invalid, err := r.discoverMembers(ctx, vnet)
	if err != nil {
		return ctrl.Result{}, err
	}

	out := Generate(GenerateInput{
		VNet:        vnet,
		LabelPrefix: r.labelPrefix(),
		MembersByNS: members,
	})

	// Server-side apply each desired policy.
	desiredKeys := make(map[string]bool, len(out.Policies))
	policyRefs := make([]vnetv1alpha1.PolicyRef, 0, len(out.Policies))
	for i := range out.Policies {
		p := &out.Policies[i]
		desiredKeys[p.Namespace+"/"+p.Name] = true
		if err := r.applyPolicy(ctx, p); err != nil {
			logger.Error(err, "apply policy failed", "policy", p.Namespace+"/"+p.Name)
			setReady(vnet, metav1.ConditionFalse, "ApplyFailed", err.Error())
			_ = r.updateStatus(ctx, vnet, members, policyRefs)
			return ctrl.Result{}, err
		}
		policyRefs = append(policyRefs, vnetv1alpha1.PolicyRef{Namespace: p.Namespace, Name: p.Name})
	}

	// Ensure baseline in every namespace that has members (and is managed).
	for ns := range members {
		if !r.NSFilter.IsManagedName(ns) {
			continue
		}
		nsObj := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: ns}, nsObj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, err
		}
		if !r.NSFilter.IsManaged(nsObj) {
			continue
		}
		if err := r.applyPolicy(ctx, DesiredBaseline(ns)); err != nil {
			logger.Error(err, "apply baseline failed", "namespace", ns)
			return ctrl.Result{}, err
		}
	}

	// Delete stale policies owned by this VirtualNetwork.
	if err := r.deleteStale(ctx, vnet, desiredKeys); err != nil {
		return ctrl.Result{}, err
	}

	// Status conditions.
	if len(invalid) > 0 {
		msg := fmt.Sprintf("%d invalid joiner(s): %s", len(invalid), summarizeInvalid(invalid))
		setDegraded(vnet, metav1.ConditionTrue, "InvalidJoiners", msg)
	} else {
		setDegraded(vnet, metav1.ConditionFalse, "NoIssues", "")
	}
	switch {
	case len(out.Policies) == 0:
		setReady(vnet, metav1.ConditionTrue, "NoMembers", "no pods are joining this VirtualNetwork")
	default:
		setReady(vnet, metav1.ConditionTrue, "PoliciesGenerated",
			fmt.Sprintf("%d NetworkPolic(y|ies) across %d namespace(s)", len(out.Policies), len(members)))
	}

	if err := r.updateStatus(ctx, vnet, members, policyRefs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// labelPrefix returns the configured label prefix or the default.
func (r *VirtualNetworkReconciler) labelPrefix() string {
	if r.LabelPrefix == "" {
		return DefaultLabelPrefix
	}
	if !strings.HasSuffix(r.LabelPrefix, "/") {
		return r.LabelPrefix + "/"
	}
	return r.LabelPrefix
}

// discoverMembers lists pods that join the given VirtualNetwork. For Namespace
// extent it only looks in the home namespace; for Cluster extent it looks
// cluster-wide for both bare and namespace-prefixed forms of the join label.
// Pods in unmanaged namespaces are skipped.
func (r *VirtualNetworkReconciler) discoverMembers(ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork) (
	members map[string][]string, invalid []InvalidJoiner, err error,
) {
	members = map[string][]string{}
	prefix := r.labelPrefix()
	bareKey := prefix + "net." + vnet.Name
	prefixedKey := prefix + "net." + vnet.Namespace + "." + vnet.Name

	if vnet.Spec.Extent == vnetv1alpha1.ExtentCluster {
		// Cluster extent: look in every namespace.
		var pods corev1.PodList
		if err := r.List(ctx, &pods); err != nil {
			return nil, nil, err
		}
		for _, p := range pods.Items {
			if !r.NSFilter.IsManagedName(p.Namespace) {
				continue
			}
			joined := false
			if p.Namespace == vnet.Namespace {
				if _, ok := p.Labels[bareKey]; ok {
					joined = true
				}
			} else if _, ok := p.Labels[prefixedKey]; ok {
				joined = true
			}
			if joined {
				members[p.Namespace] = append(members[p.Namespace], p.Name)
			}
		}
	} else {
		// Namespace extent: only home namespace, only the bare label.
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(vnet.Namespace)); err != nil {
			return nil, nil, err
		}
		for _, p := range pods.Items {
			if _, ok := p.Labels[bareKey]; ok {
				members[p.Namespace] = append(members[p.Namespace], p.Name)
			}
		}
		// Cross-namespace joiners on a Namespace-extent vnet are invalid; surface them in status.
		var allPods corev1.PodList
		if err := r.List(ctx, &allPods); err == nil {
			for _, p := range allPods.Items {
				if p.Namespace == vnet.Namespace {
					continue
				}
				if _, ok := p.Labels[prefixedKey]; ok {
					invalid = append(invalid, InvalidJoiner{
						PodNamespace: p.Namespace,
						PodName:      p.Name,
						Reason:       "VirtualNetwork has Namespace extent",
					})
				}
			}
		}
	}

	for ns := range members {
		sort.Strings(members[ns])
	}
	return members, invalid, nil
}

// applyPolicy server-side-applies a NetworkPolicy with the operator's field manager.
func (r *VirtualNetworkReconciler) applyPolicy(ctx context.Context, p *networkingv1.NetworkPolicy) error {
	// SSA requires the object's GVK and a clean ResourceVersion.
	p.SetResourceVersion("")
	return r.Patch(ctx, p, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership)
}

// deleteStale deletes any operator-managed NetworkPolicy carrying this vnet's
// network label that is not in the desired set.
func (r *VirtualNetworkReconciler) deleteStale(ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork, desired map[string]bool) error {
	netID := vnet.Namespace + "." + vnet.Name
	var existing networkingv1.NetworkPolicyList
	if err := r.List(ctx, &existing, client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelNetwork:   netID,
	}); err != nil {
		return err
	}
	for i := range existing.Items {
		p := &existing.Items[i]
		key := p.Namespace + "/" + p.Name
		if desired[key] {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// cleanupForDeleted removes all operator-managed policies for a VirtualNetwork that no longer exists.
func (r *VirtualNetworkReconciler) cleanupForDeleted(ctx context.Context, ns, name string) error {
	netID := ns + "." + name
	var policies networkingv1.NetworkPolicyList
	if err := r.List(ctx, &policies, client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelNetwork:   netID,
	}); err != nil {
		return err
	}
	for i := range policies.Items {
		p := &policies.Items[i]
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// updateStatus applies status fields to the vnet via subresource update.
func (r *VirtualNetworkReconciler) updateStatus(
	ctx context.Context,
	vnet *vnetv1alpha1.VirtualNetwork,
	members map[string][]string,
	policies []vnetv1alpha1.PolicyRef,
) error {
	out := make([]vnetv1alpha1.NamespaceMembers, 0, len(members))
	for ns, pods := range members {
		out = append(out, vnetv1alpha1.NamespaceMembers{Namespace: ns, Pods: pods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	vnet.Status.Members = out
	vnet.Status.GeneratedPolicies = policies
	vnet.Status.ObservedGeneration = vnet.Generation
	return r.Status().Update(ctx, vnet)
}

// setReady upserts the Ready condition.
func setReady(vnet *vnetv1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(vnet, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
}

// setDegraded upserts the Degraded condition.
func setDegraded(vnet *vnetv1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(vnet, metav1.Condition{
		Type:               "Degraded",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
}

func upsertCondition(vnet *vnetv1alpha1.VirtualNetwork, c metav1.Condition) {
	for i, existing := range vnet.Status.Conditions {
		if existing.Type == c.Type {
			if existing.Status != c.Status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			vnet.Status.Conditions[i] = c
			return
		}
	}
	vnet.Status.Conditions = append(vnet.Status.Conditions, c)
}

func summarizeInvalid(in []InvalidJoiner) string {
	if len(in) == 0 {
		return ""
	}
	const max = 3
	parts := make([]string, 0, max)
	for i, j := range in {
		if i >= max {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(in)-max))
			break
		}
		parts = append(parts, j.PodNamespace+"/"+j.PodName)
	}
	return strings.Join(parts, ", ")
}

// SetupWithManager wires the controller into the manager: primary VirtualNetwork
// watch + Pod watch (filtered by label prefix) + NetworkPolicy watch (filtered by managed-by).
func (r *VirtualNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	prefix := r.labelPrefix()
	podPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		for k := range obj.GetLabels() {
			if strings.HasPrefix(k, prefix+"net.") {
				return true
			}
		}
		return false
	})
	policyPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[LabelManagedBy] == LabelManagedByValue
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&vnetv1alpha1.VirtualNetwork{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToVNets),
			builder.WithPredicates(podPredicate),
		).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToVNet),
			builder.WithPredicates(policyPredicate),
		).
		Complete(r)
}

// podToVNets maps a pod event to the VirtualNetworks the pod claims to join via its labels.
// (Previously-observed memberships are caught by the periodic resync and the policy drift watch.)
func (r *VirtualNetworkReconciler) podToVNets(ctx context.Context, obj client.Object) []reconcile.Request {
	prefix := r.labelPrefix()
	keyPrefix := prefix + "net."
	var reqs []reconcile.Request
	for k := range obj.GetLabels() {
		if !strings.HasPrefix(k, keyPrefix) {
			continue
		}
		rest := strings.TrimPrefix(k, keyPrefix)
		// Bare form "net.<vnet>" → VirtualNetwork in pod's namespace.
		// Prefixed form "net.<ns>.<vnet>" → VirtualNetwork in another namespace.
		switch parts := strings.SplitN(rest, ".", 2); len(parts) {
		case 1:
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: obj.GetNamespace(), Name: parts[0],
			}})
		case 2:
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: parts[0], Name: parts[1],
			}})
		}
	}
	return reqs
}

// policyToVNet maps a managed NetworkPolicy event back to its owning VirtualNetwork.
// The kube-vnet/network label encodes "<ns>.<name>".
func (r *VirtualNetworkReconciler) policyToVNet(ctx context.Context, obj client.Object) []reconcile.Request {
	v := obj.GetLabels()[LabelNetwork]
	if v == "" {
		return nil
	}
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: parts[0], Name: parts[1]}}}
}
