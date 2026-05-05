package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Condition reasons surfaced on VirtualNetworkBinding.status.conditions.
const (
	ReasonBindingPodsAttached       = "PodsAttached"
	ReasonBindingNoPodsMatch        = "NoPodsMatch"
	ReasonBindingVNetNotFound       = "VirtualNetworkNotFound"
	ReasonBindingNamespaceNotAllowed = "NamespaceNotAllowed"
	ReasonBindingNamespaceExcluded  = "NamespaceExcluded"
	ReasonBindingUnknownDirection   = "UnknownDirection"
	ReasonBindingInvalidSelector    = "InvalidSelector"
)

// VirtualNetworkBindingReconciler maintains the binding's own status. The
// effect of the binding on NetworkPolicies is the VirtualNetworkReconciler's
// responsibility (it watches bindings via a mapper).
type VirtualNetworkBindingReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	NSFilter *NamespaceFilter
}

// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=clustervirtualnetworkbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=clustervirtualnetworkbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=clustervirtualnetworkbindings/finalizers,verbs=update

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

	// Namespace excluded → nothing to do, but reflect that on the binding.
	bns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: b.Namespace}, bns); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if bns.Name == "" || !r.NSFilter.IsManaged(bns) {
		setBindingReady(b, metav1.ConditionFalse, ReasonBindingNamespaceExcluded,
			fmt.Sprintf("namespace %q is excluded by the operator", b.Namespace))
		return ctrl.Result{}, r.writeStatus(ctx, b, nil)
	}

	// Validate direction.
	dirVal := b.Spec.Direction
	if dirVal == "" {
		dirVal = string(DirectionBoth)
	}
	if _, ok := ParseDirection(dirVal); !ok {
		setBindingReady(b, metav1.ConditionFalse, ReasonBindingUnknownDirection,
			fmt.Sprintf("unknown direction %q", dirVal))
		return ctrl.Result{}, r.writeStatus(ctx, b, nil)
	}

	// Locate target VirtualNetwork.
	vnet := &vnetv1alpha1.VirtualNetwork{}
	vnetKey := client.ObjectKey{
		Namespace: b.Spec.VirtualNetworkRef.Namespace,
		Name:      b.Spec.VirtualNetworkRef.Name,
	}
	if err := r.Get(ctx, vnetKey, vnet); err != nil {
		if apierrors.IsNotFound(err) {
			setBindingReady(b, metav1.ConditionFalse, ReasonBindingVNetNotFound,
				fmt.Sprintf("VirtualNetwork %s/%s not found", vnetKey.Namespace, vnetKey.Name))
			return ctrl.Result{}, r.writeStatus(ctx, b, nil)
		}
		return ctrl.Result{}, err
	}

	// Check vnet's allowedNamespaces permits this binding's namespace.
	allowed, err := nsPermits(ctx, r.Client, vnet, b.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allowed {
		setBindingReady(b, metav1.ConditionFalse, ReasonBindingNamespaceNotAllowed,
			fmt.Sprintf("VirtualNetwork %s/%s does not permit namespace %q",
				vnet.Namespace, vnet.Name, b.Namespace))
		return ctrl.Result{}, r.writeStatus(ctx, b, nil)
	}

	// Evaluate the binding's podSelector against pods in the binding's namespace.
	sel, err := metav1.LabelSelectorAsSelector(&b.Spec.PodSelector)
	if err != nil {
		setBindingReady(b, metav1.ConditionFalse, ReasonBindingInvalidSelector, err.Error())
		return ctrl.Result{}, r.writeStatus(ctx, b, nil)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(b.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		names = append(names, pods.Items[i].Name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		setBindingReady(b, metav1.ConditionTrue, ReasonBindingNoPodsMatch,
			"binding accepted; no pods currently match the selector")
	} else {
		setBindingReady(b, metav1.ConditionTrue, ReasonBindingPodsAttached,
			fmt.Sprintf("%d pod(s) attached to %s/%s", len(names), vnet.Namespace, vnet.Name))
	}
	if err := r.writeStatus(ctx, b, names); err != nil {
		logger.Error(err, "status update failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *VirtualNetworkBindingReconciler) writeStatus(
	ctx context.Context, b *vnetv1alpha1.VirtualNetworkBinding, attachedPods []string,
) error {
	b.Status.AttachedPods = attachedPods
	b.Status.ObservedGeneration = b.Generation
	return r.Status().Update(ctx, b)
}

func setBindingReady(b *vnetv1alpha1.VirtualNetworkBinding, status metav1.ConditionStatus, reason, msg string) {
	upsertBindingCondition(b, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg})
}

func upsertBindingCondition(b *vnetv1alpha1.VirtualNetworkBinding, c metav1.Condition) {
	now := metav1.Now()
	for i, existing := range b.Status.Conditions {
		if existing.Type == c.Type {
			if existing.Status != c.Status {
				c.LastTransitionTime = now
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			b.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = now
	b.Status.Conditions = append(b.Status.Conditions, c)
}

// nsPermits is a free-standing equivalent of VirtualNetworkReconciler.permits
// so the binding controller doesn't need to hold a reference to the vnet
// reconciler. Keeps the two reconcilers independent.
func nsPermits(ctx context.Context, c client.Client, vnet *vnetv1alpha1.VirtualNetwork, ns string) (bool, error) {
	if ns == vnet.Namespace {
		return true, nil
	}
	sel := vnet.Spec.AllowedNamespaces
	if sel == nil {
		return false, nil
	}
	if sel.All {
		return true, nil
	}
	for _, n := range sel.Names {
		if n == ns {
			return true, nil
		}
	}
	if sel.Selector != nil {
		nsObj := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: ns}, nsObj); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		s, err := metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return false, err
		}
		if s.Matches(labels.Set(nsObj.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

func (r *VirtualNetworkBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vnetv1alpha1.VirtualNetworkBinding{}).
		Watches(
			&vnetv1alpha1.VirtualNetwork{},
			handler.EnqueueRequestsFromMapFunc(r.vnetToBindings),
		).
		Complete(r)
}

// vnetToBindings enqueues every binding that targets the changed vnet.
func (r *VirtualNetworkBindingReconciler) vnetToBindings(ctx context.Context, obj client.Object) []reconcile.Request {
	v, ok := obj.(*vnetv1alpha1.VirtualNetwork)
	if !ok {
		return nil
	}
	var bindings vnetv1alpha1.VirtualNetworkBindingList
	if err := r.List(ctx, &bindings); err != nil {
		return nil
	}
	out := []reconcile.Request{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.VirtualNetworkRef.Name == v.Name && b.Spec.VirtualNetworkRef.Namespace == v.Namespace {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: b.Namespace, Name: b.Name,
			}})
		}
	}
	return out
}
