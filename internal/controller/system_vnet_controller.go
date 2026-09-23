package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// SystemVnetReconciler ensures that the per-namespace `namespace` system vnet
// exists in every managed namespace, and that the cluster-wide `cluster`
// system vnet exists in the operator's own namespace. Both are drift-corrected
// on delete.
//
// Reconciler is keyed on the cluster-scoped Namespace name. Two trigger paths:
//   - Namespace events: ensure the per-namespace `namespace` vnet (if managed)
//     and ensure the cluster vnet (if this is the operator's namespace).
//   - VirtualNetwork events filtered by LabelManagedBy: re-enqueue the namespace
//     so a deleted system vnet is recreated.
//
// See ADR 0030.
type SystemVnetReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	NSFilter          *NamespaceFilter
	OperatorNamespace string
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
		if err := r.ensureNamespaceSystemVnet(ctx, ns.Name); err != nil {
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
		if err := r.ensureClusterSystemVnet(ctx); err != nil {
			logger.Error(err, "ensure cluster system vnet failed")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *SystemVnetReconciler) ensureNamespaceSystemVnet(ctx context.Context, ns string) error {
	desired := desiredSystemVnet(SystemVnetNamespace, ns, "Per-namespace system vnet for kube-vnet (operator-managed). Pods join via kube-vnet/net.namespace.")
	return r.applySystemVnet(ctx, desired)
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
	if v.Labels[LabelManagedBy] != LabelManagedByValue {
		return nil
	}
	if err := r.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	logger.Info("deleted per-NS system vnet in disabled namespace", "vnet", v.Name)
	return nil
}

func (r *SystemVnetReconciler) ensureClusterSystemVnet(ctx context.Context) error {
	if r.OperatorNamespace == "" {
		return fmt.Errorf("operator namespace is empty; cannot create cluster system vnet")
	}
	desired := desiredSystemVnet(SystemVnetCluster, r.OperatorNamespace, "Cluster-wide system vnet for kube-vnet (operator-managed). Pods join via kube-vnet/net.cluster.")
	desired.Spec.AllowedNamespaces = &vnetv1alpha1.NamespaceSelector{All: true}
	return r.applySystemVnet(ctx, desired)
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

func (r *SystemVnetReconciler) applySystemVnet(ctx context.Context, desired *vnetv1alpha1.VirtualNetwork) error {
	desired.SetResourceVersion("")
	return r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership)
}

func (r *SystemVnetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Drift correction: an event on a system vnet re-enqueues its namespace,
	// which recreates the vnet if it was deleted.
	systemPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("system-vnet").
		For(&corev1.Namespace{}).
		Watches(
			&vnetv1alpha1.VirtualNetwork{},
			handler.EnqueueRequestsFromMapFunc(objectNamespace),
			builder.WithPredicates(systemPredicate),
		).
		Complete(r)
}
