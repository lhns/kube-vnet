package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// LabelSystem marks operator-owned "system" VirtualNetwork resources (the
// per-namespace `namespace` vnet and the cluster-wide `cluster` vnet). User
// vnets do not carry this label. See ADR 0030.
const LabelSystem = "kube-vnet/system"

// LabelSystemValue is the value placed on system vnets so they can be
// label-selected and so a future ValidatingAdmissionPolicy can reject user
// mutation of them.
const LabelSystemValue = "true"

// System vnet names. These are reserved — a user-authored VirtualNetwork with
// the same name in a managed namespace will collide with the operator-managed
// system vnet (which is recreated on delete). On upgrade, users with such
// names need to rename their vnets.
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
//   - VirtualNetwork events filtered by LabelSystem: re-enqueue the namespace
//     so a deleted system vnet is recreated.
//
// See ADR 0030.
type SystemVnetReconciler struct {
	client.Client
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	NSFilter          *NamespaceFilter
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks,verbs=get;list;watch;create;update;patch

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

	// Per-namespace `namespace` system vnet: only in managed namespaces.
	if r.NSFilter.IsManaged(ns) {
		if err := r.ensureNamespaceSystemVnet(ctx, ns.Name); err != nil {
			logger.Error(err, "ensure namespace system vnet failed")
			return ctrl.Result{}, err
		}
	}

	// Cluster system vnet: only when reconciling the operator's namespace.
	// (Reconciling on every namespace event would be redundant; we want the
	// trigger to come from the operator's namespace specifically. If the
	// cluster vnet's home namespace is unmanaged for some reason — e.g.
	// the operator runs in kube-system, which is in disabledNamespaces by
	// default — we still create it; the system vnet itself is not subject
	// to the disabled-namespaces filter.)
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
				LabelSystem:    LabelSystemValue,
				LabelManagedBy: LabelManagedByValue,
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
	// Drift-correct: a VirtualNetwork delete event labelled kube-vnet/system=true
	// re-enqueues its namespace (or the operator namespace if it was the cluster
	// vnet) so the system vnet is recreated on the next reconcile pass.
	systemPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelSystem] == LabelSystemValue
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("system-vnet").
		For(&corev1.Namespace{}).
		Watches(
			&vnetv1alpha1.VirtualNetwork{},
			handler.EnqueueRequestsFromMapFunc(systemVnetToNamespace),
			builder.WithPredicates(systemPredicate),
		).
		Complete(r)
}

// systemVnetToNamespace maps a system VirtualNetwork event back to a reconcile
// request keyed on the vnet's namespace. (The namespace reconciler is keyed
// on cluster-scoped namespace name.)
func systemVnetToNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
}
