package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Pod-event reasons emitted by JoinLabelDiagnosticReconciler. See ADR 0027.
const (
	// EventBareJoinLabelVnetNotFound: a pod carries a bare-form join label
	// `kube-vnet/net.<X>` but no VirtualNetwork named `<X>` exists in the
	// pod's own namespace. The bare form is only honored in the vnet's
	// home namespace; if the user intended a vnet elsewhere, they need
	// the prefixed form `kube-vnet/net.<homeNS>.<X>`.
	EventBareJoinLabelVnetNotFound = "BareJoinLabelVnetNotFound"

	// EventPrefixedJoinLabelVnetNotFound: a pod carries a prefixed-form
	// join label `kube-vnet/net.<homeNS>.<X>` but no VirtualNetwork
	// `<homeNS>/<X>` exists. Likely a typo in the home namespace or vnet
	// name, or the vnet hasn't been created yet.
	EventPrefixedJoinLabelVnetNotFound = "PrefixedJoinLabelVnetNotFound"

	// EventJoinLabelNamespaceNotAllowed: a pod carries a prefixed-form
	// join label and the named vnet exists, but the vnet's
	// `spec.allowedNamespaces` doesn't permit the pod's namespace. Either
	// the vnet owner needs to extend allowedNamespaces, or the pod doesn't
	// belong here.
	EventJoinLabelNamespaceNotAllowed = "JoinLabelNamespaceNotAllowed"
)

// JoinLabelDiagnosticReconciler emits Kubernetes Events on Pod objects when
// their kube-vnet join labels can't be honored at the moment of reconcile.
//
// This is the pod-owner-scoped counterpart to the vnet-owner-scoped
// `Degraded`/`InvalidJoiners` condition the VirtualNetworkReconciler maintains.
// Both surfaces fire when applicable; they serve different audiences. See
// ADR 0027.
type JoinLabelDiagnosticReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	LabelPrefix string
	NSFilter    *NamespaceFilter
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *JoinLabelDiagnosticReconciler) labelPrefix() string {
	if r.LabelPrefix == "" {
		return DefaultLabelPrefix
	}
	if !strings.HasSuffix(r.LabelPrefix, "/") {
		return r.LabelPrefix + "/"
	}
	return r.LabelPrefix
}

func (r *JoinLabelDiagnosticReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Skip pods in operator-disabled namespaces. They're explicit opt-outs;
	// emitting diagnostics would be alert fatigue.
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !r.NSFilter.IsManaged(ns) {
		return ctrl.Result{}, nil
	}

	prefix := r.labelPrefix()
	keyPrefix := prefix + "net."

	for k, v := range pod.Labels {
		if !strings.HasPrefix(k, keyPrefix) {
			continue
		}
		_ = v // direction value is validated at admission (VAP) and via vnet status
		suffix := strings.TrimPrefix(k, keyPrefix)

		// Classify by dot count: bare = "X" (no dot); prefixed = "homeNS.X" (one dot).
		// More than one dot is malformed; skip — the VirtualNetworkReconciler
		// won't honor it either.
		switch strings.Count(suffix, ".") {
		case 0:
			r.diagBare(ctx, pod, k, suffix)
		case 1:
			parts := strings.SplitN(suffix, ".", 2)
			r.diagPrefixed(ctx, pod, k, parts[0], parts[1])
		default:
			logger.V(1).Info("malformed join-label key, skipping", "key", k)
		}
	}
	return ctrl.Result{}, nil
}

// diagBare handles the bare form `kube-vnet/net.<X>`. The label is meaningful
// only when a VirtualNetwork named X exists in the pod's own namespace, OR
// when X is a reserved system-vnet name (`cluster` or `namespace`). The
// `cluster` system vnet lives in the operator NS (not in the pod's NS), so a
// naive Get-in-pod-NS would false-positive `BareJoinLabelVnetNotFound` on
// every cluster-membership pod.
func (r *JoinLabelDiagnosticReconciler) diagBare(ctx context.Context, pod *corev1.Pod, labelKey, vnetName string) {
	if vnetName == SystemVnetCluster || vnetName == SystemVnetNamespace {
		// Reserved system-vnet name; the bare label is always legitimate.
		return
	}
	v := &vnetv1alpha1.VirtualNetwork{}
	err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: vnetName}, v)
	if err == nil {
		// Vnet exists locally; bare form is legitimate. No event.
		return
	}
	if !apierrors.IsNotFound(err) {
		// Transient API error; the next reconcile will retry.
		return
	}
	r.Recorder.Eventf(pod, corev1.EventTypeWarning, EventBareJoinLabelVnetNotFound,
		"label %q points at no VirtualNetwork in namespace %q. The bare form is only honored in the vnet's home namespace. "+
			"To join a vnet hosted in another namespace, use the prefixed form %q instead.",
		labelKey, pod.Namespace,
		fmt.Sprintf("%snet.<homeNS>.%s", r.labelPrefix(), vnetName),
	)
}

// diagPrefixed handles the prefixed form `kube-vnet/net.<homeNS>.<X>`. Two
// failure modes: vnet doesn't exist; or vnet exists but doesn't permit this
// pod's namespace.
func (r *JoinLabelDiagnosticReconciler) diagPrefixed(ctx context.Context, pod *corev1.Pod, labelKey, homeNS, vnetName string) {
	v := &vnetv1alpha1.VirtualNetwork{}
	err := r.Get(ctx, client.ObjectKey{Namespace: homeNS, Name: vnetName}, v)
	if apierrors.IsNotFound(err) {
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, EventPrefixedJoinLabelVnetNotFound,
			"label %q references VirtualNetwork %q which does not exist. Check the home namespace and vnet name for typos, or create the vnet.",
			labelKey, fmt.Sprintf("%s/%s", homeNS, vnetName),
		)
		return
	}
	if err != nil {
		// Transient; retry on next reconcile.
		return
	}
	// Vnet exists. Is this pod's namespace permitted?
	if pod.Namespace == homeNS {
		// Long-form-in-home (ADR 0022) — always permitted.
		return
	}
	if r.permits(ctx, v, pod.Namespace) {
		return
	}
	r.Recorder.Eventf(pod, corev1.EventTypeWarning, EventJoinLabelNamespaceNotAllowed,
		"label %q references VirtualNetwork %q, but its spec.allowedNamespaces does not permit namespace %q. "+
			"Either extend allowedNamespaces on the vnet or move the pod to a permitted namespace.",
		labelKey, fmt.Sprintf("%s/%s", homeNS, vnetName), pod.Namespace,
	)
}

// permits mirrors VirtualNetworkReconciler.permits (free-standing so this
// reconciler doesn't depend on the vnet reconciler).
func (r *JoinLabelDiagnosticReconciler) permits(ctx context.Context, v *vnetv1alpha1.VirtualNetwork, podNS string) bool {
	if podNS == v.Namespace {
		return true
	}
	sel := v.Spec.AllowedNamespaces
	if sel == nil {
		return false
	}
	if sel.All {
		return true
	}
	for _, n := range sel.Names {
		if n == podNS {
			return true
		}
	}
	if sel.Selector != nil {
		nsObj := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: podNS}, nsObj); err != nil {
			return false
		}
		s, err := metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return false
		}
		if s.Matches(labels.Set(nsObj.Labels)) {
			return true
		}
	}
	return false
}

func (r *JoinLabelDiagnosticReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("joinlabel-diagnostic").
		For(&corev1.Pod{}, builder.WithPredicates(JoinLabelPodPredicate(r.labelPrefix()))).
		Complete(r)
}
