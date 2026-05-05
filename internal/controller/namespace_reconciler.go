package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NamespaceReconciler is the *sole owner* of the
// kube-vnet-default-deny baseline lifecycle. The baseline is decided by the
// resolved IsolationMode for each namespace (operator-level default + per-mode
// override lists + per-namespace `kube-vnet/ingress-isolation` annotation),
// not by membership presence — see ADR 0023.
//
// For each Namespace event:
//   - If the namespace is excluded (`--disabled-namespaces`) or annotated
//     `kube-vnet/disabled=true`, ensure no baseline is present.
//   - Otherwise resolve the isolation mode and apply (or remove) the baseline
//     accordingly.
//
// The reconciler also watches `NetworkPolicy` events scoped to baseline
// policies (label `kube-vnet/role=baseline`) so a manual delete of the
// baseline is detected and the policy is re-applied within one reconcile
// cycle. This mirrors the drift-correction behavior the
// VirtualNetworkReconciler provides for membership policies.
type NamespaceReconciler struct {
	client.Client
	APIReader        client.Reader
	Scheme           *runtime.Scheme
	NSFilter         *NamespaceFilter
	BaselineElideFor []string // vnet keys whose receivers are excluded from the baseline
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

	// Disabled namespaces get no kube-vnet objects at all — bypass
	// DesiredBaseline (which now always returns a non-nil policy) and sweep
	// any leftovers.
	if !r.NSFilter.IsManaged(ns) {
		var existing networkingv1.NetworkPolicyList
		if err := r.List(ctx, &existing,
			client.InNamespace(ns.Name),
			client.MatchingLabels{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleBaseline},
		); err != nil {
			return ctrl.Result{}, err
		}
		for i := range existing.Items {
			if err := r.Delete(ctx, &existing.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	desired := DesiredBaseline(ns.Name, r.BaselineElideFor)

	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply baseline failed")
		return ctrl.Result{}, err
	}

	// Sweep stale baselines: any policy in this namespace labelled as a
	// kube-vnet-managed baseline whose name is not the current
	// BaselinePolicyName is a leftover from a previous version (the constant
	// changed in the policy-name-collisions rename — old name was
	// `kube-vnet-default-deny`). Generic by name comparison so future
	// renames are handled by the same code path.
	if err := r.sweepStaleBaselines(ctx, ns.Name); err != nil {
		logger.Error(err, "sweep stale baselines failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// sweepStaleBaselines deletes policies in `ns` that are labelled as a
// kube-vnet-managed baseline but whose name differs from the current
// BaselinePolicyName. Cheap (server-side label-filtered List).
func (r *NamespaceReconciler) sweepStaleBaselines(ctx context.Context, ns string) error {
	logger := log.FromContext(ctx).WithValues("namespace", ns)
	var existing networkingv1.NetworkPolicyList
	if err := r.List(ctx, &existing,
		client.InNamespace(ns),
		client.MatchingLabels{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleBaseline},
	); err != nil {
		return err
	}
	for i := range existing.Items {
		p := &existing.Items[i]
		if p.Name == BaselinePolicyName {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		logger.Info("deleted stale baseline", "policy", p.Name, "current", BaselinePolicyName)
	}
	return nil
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Match operator-managed baseline policies so a manual delete of
	// kube-vnet-default-deny enqueues the namespace for re-reconcile.
	baselinePredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue && l[LabelRole] == LabelRoleBaseline
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(baselinePolicyToNamespace),
			builder.WithPredicates(baselinePredicate),
		).
		Complete(r)
}

// baselinePolicyToNamespace maps a baseline NetworkPolicy event back to a
// reconcile request keyed on the policy's namespace (NamespaceReconciler is
// keyed on the cluster-scoped namespace name).
func baselinePolicyToNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
}
