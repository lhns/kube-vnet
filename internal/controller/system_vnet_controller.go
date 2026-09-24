package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// System vnet names. These are reserved for the operator-managed system
// vnets; the reserved-name VAP rejects user-authored vnets with them.
const (
	SystemVnetNamespace = "namespace"
	SystemVnetCluster   = "cluster"
)

// SystemVnetReconciler keeps the `namespace` system vnet in every managed
// namespace and the `cluster` system vnet in the operator's namespace (ADR
// 0030). It is keyed by Namespace; an event on a system vnet re-enqueues its
// namespace, so a deleted one is recreated.
type SystemVnetReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	NSFilter          *NamespaceFilter
	OperatorNamespace string
	// Recorder surfaces a failed create on the system vnet. Optional.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks,verbs=get;list;watch;create;update;patch;delete

func (r *SystemVnetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Name)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace gone — apiserver garbage-collects in-namespace resources.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Don't recreate the vnet in a terminating namespace: it would fight the
	// teardown and fail NamespaceLifecycle admission.
	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// The per-namespace `namespace` vnet exists in managed namespaces only
	// (ADR 0033). The operator namespace is unmanaged but holds `cluster`,
	// handled below.
	if r.NSFilter.IsManaged(ns) {
		desired := desiredSystemVnet(SystemVnetNamespace, ns.Name,
			"Per-namespace system vnet for kube-vnet (operator-managed). Pods join via kube-vnet/net.namespace.")
		if err := r.applySystemVnet(ctx, desired,
			"pods in this namespace that join `namespace` are not members of it until this is fixed"); err != nil {
			logger.Error(err, "ensure namespace system vnet failed")
			return ctrl.Result{}, err
		}
	} else if ns.Name != r.OperatorNamespace {
		if err := r.deleteNamespaceSystemVnet(ctx, ns.Name); err != nil {
			logger.Error(err, "delete namespace system vnet in disabled namespace failed")
			return ctrl.Result{}, err
		}
	}

	// The cluster vnet lives in the operator namespace. That namespace is
	// unmanaged, so VirtualNetworkReconciler exempts system vnets from its
	// home-namespace check.
	if ns.Name == r.OperatorNamespace {
		desired := desiredSystemVnet(SystemVnetCluster, r.OperatorNamespace,
			"Cluster-wide system vnet for kube-vnet (operator-managed). Pods join via kube-vnet/net.cluster.")
		desired.Spec.AllowedNamespaces = &vnetv1alpha1.NamespaceSelector{All: true}
		if err := r.applySystemVnet(ctx, desired,
			"pods that join `cluster` are not members of it until this is fixed"); err != nil {
			logger.Error(err, "ensure cluster system vnet failed")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// deleteNamespaceSystemVnet deletes the per-namespace `namespace` system vnet
// in a disabled namespace (ADR 0033).
func (r *SystemVnetReconciler) deleteNamespaceSystemVnet(ctx context.Context, ns string) error {
	logger := log.FromContext(ctx).WithValues("namespace", ns)
	v := &vnetv1alpha1.VirtualNetwork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemVnetNamespace}, v); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	// Only delete what the operator created, in case the reserved-name VAP
	// is absent or disabled.
	if !operatorManaged(v) {
		return nil
	}
	if err := r.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	logger.Info("deleted per-NS system vnet in disabled namespace", "vnet", v.Name)
	return nil
}

func desiredSystemVnet(name, namespace, description string) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: vnetv1alpha1.GroupVersion.String(),
			Kind:       "VirtualNetwork",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				LabelManagedBy:    LabelManagedByValue,
				LabelK8sManagedBy: LabelManagedByValue,
			},
		},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			Description: description,
		},
	}
}

// applySystemVnet applies desired. A failure is counted and surfaced as a
// Warning on the vnet, in its namespace; impact says what is broken meanwhile.
func (r *SystemVnetReconciler) applySystemVnet(ctx context.Context, desired *vnetv1alpha1.VirtualNetwork, impact string) error {
	desired.SetResourceVersion("")
	err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		applyFailed(r.Recorder, desired, ApplyErrorSystemVnet,
			"the system VirtualNetwork %s/%s could not be created or updated: %v; %s.",
			desired.Namespace, desired.Name, err, impact)
	}
	return err
}

func (r *SystemVnetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("system-vnet").
		For(&corev1.Namespace{}).
		Watches(
			&vnetv1alpha1.VirtualNetwork{},
			handler.EnqueueRequestsFromMapFunc(objectNamespace),
			builder.WithPredicates(predicate.NewPredicateFuncs(operatorManaged)),
		).
		Complete(r)
}
