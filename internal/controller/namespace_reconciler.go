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

// NamespaceReconciler is the *sole owner* of the
// kube-vnet-default-deny baseline lifecycle. The baseline is decided by the
// resolved IsolationMode for each namespace (operator-level default + per-mode
// override lists + per-namespace `kube-vnet/ingress-isolation` annotation),
// not by membership presence — see ADR 0023.
//
// For each Namespace event:
//   - If the namespace is excluded (`--excluded-namespaces`) or annotated
//     `kube-vnet/disabled=true`, ensure no baseline is present.
//   - Otherwise resolve the isolation mode and apply (or remove) the baseline
//     accordingly.
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

	mode := IsolationNone
	if r.NSFilter.IsManaged(ns) {
		mode = r.NSFilter.ResolveIsolation(ns)
	}

	desired := DesiredBaseline(ns.Name, mode)
	if desired == nil {
		// No baseline wanted in this namespace. Delete any leftover.
		bp := &networkingv1.NetworkPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: BaselinePolicyName}, bp); err != nil {
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

	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply baseline failed", "mode", string(mode))
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Complete(r)
}
