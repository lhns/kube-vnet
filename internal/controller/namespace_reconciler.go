package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
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
	// Recorder surfaces apply failures and restores on the baseline policy.
	// Optional.
	Recorder events.EventRecorder

	restores policyTracker
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Name)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace gone — apiserver garbage-collects in-namespace resources.
			r.restores.forgetNamespace(req.Name, nil)
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
		r.restores.forgetNamespace(ns.Name, nil)
		return ctrl.Result{}, sweepStalePolicies(ctx, r.Client, baselines, nil, nil)
	}

	desired := DesiredBaseline(ns.Name)
	created, err := applyPolicy(ctx, r.Client, r.Client, desired)
	if err != nil {
		logger.Error(err, "apply baseline failed")
		applyErrors.WithLabelValues(ApplyErrorBaseline).Inc()
		eventf(r.Recorder, desired, corev1.EventTypeWarning, EventApplyFailed, "Apply",
			"the baseline NetworkPolicy %s could not be applied: %v. While this persists the namespace has "+
				"no kube-vnet default-deny: its pods accept traffic from anywhere (fail-open). Check "+
				"ResourceQuotas and admission policies on NetworkPolicies in this namespace.", desired.Name, err)
		return ctrl.Result{}, err
	}
	if r.restores.applied(client.ObjectKeyFromObject(desired), created) {
		eventf(r.Recorder, desired, corev1.EventTypeWarning, EventPolicyRestored, "Restore",
			"this NetworkPolicy was deleted and has been recreated: it is the namespace's kube-vnet default-deny "+
				"baseline. An administrator opts a namespace out with the annotation kube-vnet/disabled=true.")
	}

	// Sweep baseline-labelled policies under any other name (e.g. from an
	// older naming scheme).
	keep := map[client.ObjectKey]bool{client.ObjectKeyFromObject(desired): true}
	return ctrl.Result{}, sweepStalePolicies(ctx, r.Client, baselines, keep, nil)
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
