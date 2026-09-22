package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HostPortReconciler emits external-allow NetworkPolicies for pods that
// declare `hostPort` (ADR 0040). NetworkPolicy can only select those pods by
// label, so ResolutionReconciler stamps
// `kube-vnet.system/host-port.<port>.<proto>=true` on them and this
// reconciler emits one policy per (namespace, port, protocol) selecting that
// stamp. Keying on the port rather than the pod means rollouts, which replace
// pods, cause no policy churn.
//
// Opting a Namespace out (`kube-vnet/disabled=true` or
// `kube-vnet/external-allow=false`) deletes its host-port policies.
type HostPortReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=create;update;patch;delete;get;list;watch

// hostPortKey identifies one (port, protocol) exposure within a namespace.
// Used as the desired-set key and as input to the policy hash.
type hostPortKey struct {
	port     int32
	protocol corev1.Protocol
}

func (k hostPortKey) String() string {
	return fmt.Sprintf("%d.%s", k.port, strings.ToLower(string(k.protocol)))
}

func (r *HostPortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Name)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// NS gone — apiserver GC cascade-deletes its policies.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Opt-out is Namespace-wide only: the policies are per namespace, not
	// per pod.
	if !r.NSFilter.IsManaged(ns) || ExternalAllowOptedOut(ns.Annotations) {
		return ctrl.Result{}, r.deleteAllInNamespace(ctx, ns.Name)
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns.Name)); err != nil {
		return ctrl.Result{}, err
	}
	desired := desiredHostPortKeys(pods.Items)

	for key := range desired {
		pol := buildHostPortPolicy(ns.Name, key)
		pol.SetResourceVersion("")
		if err := r.Patch(ctx, pol, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			logger.Error(err, "apply host-port policy failed", "key", key)
			return ctrl.Result{}, err
		}
	}

	// Sweep host-source policies whose (port, protocol) is no longer
	// declared.
	keep := make(map[client.ObjectKey]bool, len(desired))
	for key := range desired {
		keep[client.ObjectKey{Namespace: ns.Name, Name: hostPortPolicyName(ns.Name, key)}] = true
	}
	if err := sweepStalePolicies(ctx, r.Client,
		inNamespacePolicyLabels(ns.Name, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: LabelSourceKindHost,
		}),
		keep,
	); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// deleteAllInNamespace removes every host-source external-allow policy in
// the namespace.
func (r *HostPortReconciler) deleteAllInNamespace(ctx context.Context, ns string) error {
	return sweepStalePolicies(ctx, r.Client,
		inNamespacePolicyLabels(ns, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: LabelSourceKindHost,
		}),
		nil, // nothing to keep — sweep them all
	)
}

// desiredHostPortKeys returns the set of distinct (port, protocol) tuples
// declared via hostPort on any container in any pod in the namespace.
func desiredHostPortKeys(pods []corev1.Pod) map[hostPortKey]bool {
	out := map[hostPortKey]bool{}
	for _, p := range pods {
		if p.Spec.HostNetwork {
			// Out of scope (ADR 0040): most CNIs don't enforce
			// NetworkPolicy on hostNetwork pods.
			continue
		}
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.HostPort == 0 {
					continue
				}
				proto := cp.Protocol
				if proto == "" {
					proto = corev1.ProtocolTCP
				}
				out[hostPortKey{port: cp.HostPort, protocol: proto}] = true
			}
		}
	}
	return out
}

// buildHostPortPolicy constructs the policy for one (namespace, port,
// protocol): pods carrying the matching host-port stamp accept that port
// from `0.0.0.0/0`.
func buildHostPortPolicy(ns string, key hostPortKey) *networkingv1.NetworkPolicy {
	stamp := LabelSystemHostPortPrefix + key.String()
	portIS := intstr.FromInt32(key.port)
	proto := key.protocol
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostPortPolicyName(ns, key),
			Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy:    LabelManagedByValue,
				LabelK8sManagedBy: LabelManagedByValue,
				LabelRole:         LabelRoleExternalAllow,
				LabelSourceKind:   LabelSourceKindHost,
				LabelSource:       fmt.Sprintf("host-%d-%s", key.port, strings.ToLower(string(key.protocol))),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{stamp: "true"},
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &proto, Port: &portIS},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
}

// hostPortPolicyName returns `kube-vnet.ext.host.<port>.<proto>-<8hex>`
// (ADR 0039/0040).
func hostPortPolicyName(ns string, key hostPortKey) string {
	const prefix = "kube-vnet." + PolicyKindExternal + "." + PolicySourceKindHostPort + "."
	const hashLen = 8
	identity := key.String() // e.g. "8080.tcp"
	h := sha256.Sum256([]byte(ns + "/" + identity))
	return prefix + identity + "-" + hex.EncodeToString(h[:])[:hashLen]
}

func (r *HostPortReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("host-port").
		For(&corev1.Namespace{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(objectNamespace),
			builder.WithPredicates(HostPortChangedPredicate()),
		).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(objectNamespace),
			builder.WithPredicates(externalAllowPolicyPredicate(LabelSourceKindHost)),
		).
		Complete(r)
}

// HostPortChangedPredicate fires only when a pod's derived hostPort key set
// changes, so status updates on hostPort pods don't enqueue their namespace.
// Deriving through desiredHostPortKeys also ignores hostNetwork pods.
func HostPortChangedPredicate() predicate.Predicate {
	keysOf := func(obj client.Object) map[hostPortKey]bool {
		pod, ok := obj.(*corev1.Pod)
		if !ok || pod == nil {
			return map[hostPortKey]bool{}
		}
		return desiredHostPortKeys([]corev1.Pod{*pod})
	}
	has := func(obj client.Object) bool { return len(keysOf(obj)) > 0 }

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return has(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return has(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return has(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !maps.Equal(keysOf(e.ObjectOld), keysOf(e.ObjectNew))
		},
	}
}

// objectNamespace maps a namespaced object to a request for its Namespace.
func objectNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
}
