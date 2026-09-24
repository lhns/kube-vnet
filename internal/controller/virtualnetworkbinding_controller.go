package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Condition reasons surfaced on VirtualNetworkBinding.status.conditions.
const (
	ReasonBindingPodsAttached = "PodsAttached"
	ReasonBindingNoPodsMatch  = "NoPodsMatch"
	// The target vnet does not exist or does not permit the binding's
	// namespace: the same reason as the binding's Event, the message says
	// which.
	ReasonBindingVNetNotJoinable   = ReasonVirtualNetworkNotJoinable
	ReasonBindingNamespaceExcluded = "NamespaceExcluded"
	ReasonBindingInvalidDirection  = ReasonInvalidDirection
	ReasonBindingInvalidSelector   = "InvalidSelector"
	// The selector matches pods, but resolution made none of them a member
	// (a baseline or pod-label conflict, direction none, ...).
	ReasonBindingNoPodsAttached = "NoPodsAttached"
	// The target vnet's home namespace is excluded or disabled, so the vnet
	// is not served and has no membership policies.
	ReasonBindingHomeNamespaceExcluded = "HomeNamespaceExcluded"
	// The target vnet is being deleted; its membership policies are gone.
	ReasonBindingVNetTerminating = "VirtualNetworkTerminating"
)

// VirtualNetworkBindingReconciler maintains the binding's own status. The
// binding's effect on membership comes from resolution, which stamps the
// selected pods.
type VirtualNetworkBindingReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
	// OperatorNamespace is where the `cluster` system vnet lives; a ref to it
	// that omits the namespace resolves there.
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings/finalizers,verbs=update

func (r *VirtualNetworkBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("binding", req.NamespacedName)

	b := &vnetv1alpha1.VirtualNetworkBinding{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// The status as fetched, before setBindingReady changes it.
	stored := b.Status.DeepCopy()
	notReady := func(reason, msg string, args ...any) (ctrl.Result, error) {
		setBindingReady(b, metav1.ConditionFalse, reason, fmt.Sprintf(msg, args...))
		return ctrl.Result{}, r.writeStatus(ctx, b, stored, nil)
	}

	// Namespace excluded → nothing to do, but reflect that on the binding.
	if managed, err := r.NSFilter.Manages(ctx, r.Client, b.Namespace); err != nil {
		return ctrl.Result{}, err
	} else if !managed {
		return notReady(ReasonBindingNamespaceExcluded, "namespace %q is excluded by the operator", b.Namespace)
	}

	// Validate direction.
	dirVal := b.Spec.Direction
	if dirVal == "" {
		dirVal = string(DirectionBoth)
	}
	if _, ok := ParseBareDirection(dirVal); !ok {
		return notReady(ReasonBindingInvalidDirection, "spec.direction %q is not one of both, ingress, egress, none", dirVal)
	}

	// Locate target VirtualNetwork.
	vnet := &vnetv1alpha1.VirtualNetwork{}
	vnetKey := bindingTarget(b, r.OperatorNamespace)
	if err := r.Get(ctx, vnetKey, vnet); err != nil {
		if apierrors.IsNotFound(err) {
			return notReady(ReasonBindingVNetNotJoinable, "VirtualNetwork %s/%s does not exist", vnetKey.Namespace, vnetKey.Name)
		}
		return ctrl.Result{}, err
	}
	if !vnet.DeletionTimestamp.IsZero() {
		return notReady(ReasonBindingVNetTerminating, "VirtualNetwork %s/%s is being deleted", vnet.Namespace, vnet.Name)
	}

	// The vnet reconciler does not serve a vnet whose home namespace is
	// unmanaged (system vnets exempt, as there), so no pod is a member of it
	// whatever resolution stamps. The binding's own namespace was checked above.
	if !operatorManaged(vnet) && vnet.Namespace != b.Namespace {
		if managed, err := r.NSFilter.Manages(ctx, r.Client, vnet.Namespace); err != nil {
			return ctrl.Result{}, err
		} else if !managed {
			return notReady(ReasonBindingHomeNamespaceExcluded,
				"VirtualNetwork %s/%s is not served: its home namespace %q is excluded by the operator",
				vnet.Namespace, vnet.Name, vnet.Namespace)
		}
	}

	// Check vnet's allowedNamespaces permits this binding's namespace.
	allowed, err := PermitsForVnet(ctx, r.Client, vnet, b.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allowed {
		return notReady(ReasonBindingVNetNotJoinable,
			"VirtualNetwork %s/%s does not permit namespace %q; its owner can add it to spec.allowedNamespaces",
			vnet.Namespace, vnet.Name, b.Namespace)
	}

	// Evaluate the binding's podSelector against pods in the binding's namespace.
	sel, err := metav1.LabelSelectorAsSelector(&b.Spec.PodSelector)
	if err != nil {
		return notReady(ReasonBindingInvalidSelector, "%s", err.Error())
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(b.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	// Only pods resolution made members count as attached: selection is one
	// input to resolution, which baselines, pod labels or direction none can
	// override. The test mirrors discoverMembers.
	stampKey := SystemLabelKey(vnet.Namespace, vnet.Name)
	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		if isStampedMember(&pods.Items[i], stampKey) {
			names = append(names, pods.Items[i].Name)
		}
	}
	sort.Strings(names)

	selected, attached := len(pods.Items), len(names)
	switch {
	case selected == 0:
		setBindingReady(b, metav1.ConditionTrue, ReasonBindingNoPodsMatch,
			"binding accepted; no pods currently match the selector")
	case attached == 0:
		setBindingReady(b, metav1.ConditionTrue, ReasonBindingNoPodsAttached,
			fmt.Sprintf("%d pod(s) match the selector, but none is a member of %s/%s; see the pods' events and %s label",
				selected, vnet.Namespace, vnet.Name, stampKey))
	case attached < selected:
		setBindingReady(b, metav1.ConditionTrue, ReasonBindingPodsAttached,
			fmt.Sprintf("%d of %d selected pod(s) are members of %s/%s; see the other pods' events and %s label",
				attached, selected, vnet.Namespace, vnet.Name, stampKey))
	default:
		setBindingReady(b, metav1.ConditionTrue, ReasonBindingPodsAttached,
			fmt.Sprintf("%d pod(s) attached to %s/%s", attached, vnet.Namespace, vnet.Name))
	}
	if err := r.writeStatus(ctx, b, stored, names); err != nil {
		logger.Error(err, "status update failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *VirtualNetworkBindingReconciler) writeStatus(
	ctx context.Context, b *vnetv1alpha1.VirtualNetworkBinding,
	stored *vnetv1alpha1.VirtualNetworkBindingStatus, attachedPods []string,
) error {
	b.Status.AttachedPods = attachedPods
	b.Status.ObservedGeneration = b.Generation
	// Skip no-op writes: every pod change in the namespace re-enqueues the
	// binding.
	if equality.Semantic.DeepEqual(stored, &b.Status) {
		return nil
	}
	return r.Status().Update(ctx, b)
}

// isStampedMember reports whether resolution has made pod a member of the vnet
// whose stamp key is stampKey: resolved, and stamped with a direction other
// than none.
func isStampedMember(pod *corev1.Pod, stampKey string) bool {
	if pod.Annotations[AnnotationResolvedGeneration] == "" {
		return false
	}
	dir, ok := ParseBareDirection(pod.Labels[stampKey])
	return ok && dir != DirectionNone
}

func setBindingReady(b *vnetv1alpha1.VirtualNetworkBinding, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg})
}

func (r *VirtualNetworkBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vnetv1alpha1.VirtualNetworkBinding{}).
		Watches(
			&vnetv1alpha1.VirtualNetwork{},
			handler.EnqueueRequestsFromMapFunc(r.vnetToBindings),
		).
		// The reconcile also reads pods (selection and membership stamps, for
		// status.attachedPods), the binding's namespace (IsManaged, and the
		// labels PermitsForVnet may match on) and the target vnet's home
		// namespace (IsManaged). Pod events are unfiltered, so a stamp change
		// fires. See ADR 0044.
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.bindingsInNamespaceOf),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.nsToBindings),
		).
		Complete(r)
}

// bindingsInNamespaceOf enqueues every binding in the changed pod's namespace.
func (r *VirtualNetworkBindingReconciler) bindingsInNamespaceOf(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.bindingRequests(ctx, nil, client.InNamespace(obj.GetNamespace()))
}

// nsToBindings enqueues the bindings in the changed namespace and those whose
// target vnet lives there.
func (r *VirtualNetworkBindingReconciler) nsToBindings(ctx context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetName()
	return r.bindingRequests(ctx, func(b *vnetv1alpha1.VirtualNetworkBinding) bool {
		return b.Namespace == ns || bindingTarget(b, r.OperatorNamespace).Namespace == ns
	})
}

// vnetToBindings enqueues every binding that targets the changed vnet.
func (r *VirtualNetworkBindingReconciler) vnetToBindings(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.bindingRequests(ctx, func(b *vnetv1alpha1.VirtualNetworkBinding) bool {
		return bindingTarget(b, r.OperatorNamespace) == client.ObjectKeyFromObject(obj)
	})
}

// bindingRequests returns a request per binding listed with opts that passes
// keep (all if keep is nil).
func (r *VirtualNetworkBindingReconciler) bindingRequests(
	ctx context.Context, keep func(*vnetv1alpha1.VirtualNetworkBinding) bool, opts ...client.ListOption,
) []reconcile.Request {
	var bindings vnetv1alpha1.VirtualNetworkBindingList
	if err := r.List(ctx, &bindings, opts...); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range bindings.Items {
		if b := &bindings.Items[i]; keep == nil || keep(b) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
		}
	}
	return out
}

// bindingTarget returns the VirtualNetwork a binding refers to, inferring an
// omitted ref namespace as canonicalVnetKey does: the binding's own namespace,
// or operatorNS for the `cluster` singleton.
func bindingTarget(b *vnetv1alpha1.VirtualNetworkBinding, operatorNS string) client.ObjectKey {
	ref := b.Spec.VirtualNetworkRef
	ns := ref.Namespace
	if ns == "" {
		ns = b.Namespace
		if ref.Name == SystemVnetCluster {
			ns = operatorNS
		}
	}
	return client.ObjectKey{Namespace: ns, Name: ref.Name}
}
