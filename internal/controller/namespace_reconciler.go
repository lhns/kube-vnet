package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NamespaceReconciler maintains the kube-vnet-default-deny baseline in every
// non-excluded, non-disabled namespace when DefaultDenyEverywhere is enabled.
//
// It owns the *flag-driven* baseline lifecycle. The per-VirtualNetwork
// reconciler still owns the membership-driven baseline (a baseline appears as
// soon as a member shows up regardless of this flag). Both produce identical
// kube-vnet-default-deny policies; SSA with the same FieldOwner reconciles
// their writes idempotently.
//
// When the flag is false this reconciler is a no-op for every event.
type NamespaceReconciler struct {
	client.Client
	APIReader              client.Reader
	Scheme                 *runtime.Scheme
	NSFilter               *NamespaceFilter
	DefaultDenyEverywhere  bool
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if !r.DefaultDenyEverywhere {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithValues("namespace", req.Name)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace gone — nothing for us to do; the apiserver garbage-
			// collects in-namespace resources.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if r.NSFilter.IsManaged(ns) {
		// Managed namespace: ensure the baseline is present.
		baseline := DesiredBaseline(req.Name)
		baseline.SetResourceVersion("")
		if err := r.Patch(ctx, baseline, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			logger.Error(err, "apply baseline failed (default-deny-everywhere)")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Unmanaged namespace (excluded list or kube-vnet/disabled=true). Remove
	// the baseline IF no operator-managed membership policy still references
	// it — otherwise the per-vnet reconciler still wants it to exist.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var members networkingv1.NetworkPolicyList
	if err := reader.List(ctx, &members,
		client.InNamespace(req.Name),
		client.MatchingLabels{
			LabelManagedBy: LabelManagedByValue,
			LabelRole:      LabelRoleMembership,
		},
	); err != nil {
		return ctrl.Result{}, err
	}
	if len(members.Items) > 0 {
		// Membership policy still here, but the namespace went unmanaged —
		// that's an inconsistency the per-vnet reconciler will resolve on
		// its next pass. Don't touch the baseline now.
		return ctrl.Result{}, nil
	}
	bp := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Name, Name: BaselinePolicyName}, bp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if err := r.Delete(ctx, bp); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Complete(r)
}
