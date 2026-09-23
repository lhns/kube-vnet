package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// NamespaceReconciler is the sole owner of the baseline NetworkPolicy: the
// deny-all ingress policy (DesiredBaseline) in every managed namespace, and
// none in unmanaged ones. It also watches baseline policies, so a deleted
// baseline is re-applied.
type NamespaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Name)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace gone — apiserver garbage-collects in-namespace resources.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Don't re-apply into a terminating namespace: NamespaceLifecycle
	// admission would reject it, logging an error on every deletion.
	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	baselines := inNamespacePolicyLabels(ns.Name, map[string]string{LabelRole: LabelRoleBaseline})

	// Disabled namespaces get no baseline: sweep any leftover.
	if !r.NSFilter.IsManaged(ns) {
		return ctrl.Result{}, sweepStalePolicies(ctx, r.Client, baselines, nil)
	}

	desired := DesiredBaseline(ns.Name)
	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply baseline failed")
		applyErrors.WithLabelValues(ApplyErrorBaseline).Inc()
		return ctrl.Result{}, err
	}

	// Sweep baseline-labelled policies under any other name (e.g. from an
	// older naming scheme).
	keep := map[client.ObjectKey]bool{client.ObjectKeyFromObject(desired): true}
	return ctrl.Result{}, sweepStalePolicies(ctx, r.Client, baselines, keep)
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Match operator-managed baseline policies so a manual delete of the
	// baseline enqueues the namespace for re-reconcile.
	baselinePredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue && l[LabelRole] == LabelRoleBaseline
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(objectNamespace),
			builder.WithPredicates(baselinePredicate),
		).
		Complete(r)
}
