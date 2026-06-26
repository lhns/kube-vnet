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

// NamespaceReconciler is the *sole owner* of the baseline NetworkPolicy
// lifecycle. Per ADR 0030 the baseline is uniformly deny-all selecting every
// pod in every managed namespace; there are no per-mode shapes and no
// elide-list exemptions (ADR 0035 removed the elide flag — it had no
// observable effect on connectivity since NetworkPolicy union semantics
// make the baseline's deny-all redundant for pods that are already covered
// by a membership policy's allows).
//
// For each Namespace event:
//   - If the namespace is excluded (`--disabled-namespaces`) or annotated
//     `kube-vnet/disabled=true`, ensure no baseline is present.
//   - Otherwise apply the deny-all baseline.
//
// The reconciler also watches `NetworkPolicy` events scoped to baseline
// policies (label `kube-vnet.system/role=baseline`) so a manual delete of the
// baseline is detected and the policy is re-applied within one reconcile
// cycle. This mirrors the drift-correction behavior the
// VirtualNetworkReconciler provides for membership policies.
type NamespaceReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	NSFilter  *NamespaceFilter
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

	desired := DesiredBaseline(ns.Name)

	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply baseline failed")
		return ctrl.Result{}, err
	}

	// Self-heal: any baseline-labeled policy in this NS whose name doesn't
	// match the desired one (legacy `kube-vnet` literals, renamed baselines,
	// orphans from a previous reconciler version) gets swept by name.
	keep := map[client.ObjectKey]bool{
		{Namespace: ns.Name, Name: BaselinePolicyName}: true,
	}
	if err := sweepStalePolicies(ctx, r.Client,
		inNamespacePolicyLabels(ns.Name, map[string]string{LabelRole: LabelRoleBaseline}),
		keep,
	); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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
