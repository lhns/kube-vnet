package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HostPortReconciler emits one external-allow NetworkPolicy per (namespace,
// port, protocol) that a pod declares as `hostPort` (ADR 0040), selecting
// the host-port stamp resolution puts on those pods and allowing the
// containerPort(s) mapped to it: NetworkPolicy ports are evaluated after the
// hostPort DNAT. Keying on the port, not the pod, keeps rollouts from churning
// policies. A namespace opted out (`kube-vnet/disabled=true` or
// `kube-vnet/external-allow=false`) gets none.
type HostPortReconciler struct {
	client.Client
	NSFilter *NamespaceFilter
	// Recorder surfaces apply failures and restores on the policies.
	// Optional.
	Recorder events.EventRecorder

	restores policyTracker
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
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace deletion removes its policies.
			r.restores.forgetNamespace(req.Name, nil)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// NamespaceLifecycle admission rejects creates in a terminating namespace.
	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	hostPolicies := inNamespacePolicyLabels(ns.Name, map[string]string{
		LabelRole:       LabelRoleExternalAllow,
		LabelSourceKind: LabelSourceKindHost,
	})

	// Opt-out is Namespace-wide only: the policies are per namespace, not
	// per pod.
	if !r.NSFilter.IsManaged(ns) || ExternalAllowOptedOut(ns.Annotations) {
		r.restores.forgetNamespace(ns.Name, nil)
		return ctrl.Result{}, sweepStalePolicies(ctx, r.Client, hostPolicies, nil, nil)
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns.Name)); err != nil {
		return ctrl.Result{}, err
	}

	// A failed apply doesn't stop the loop, and its key stays kept so the
	// sweep leaves a live policy of that port alone, and so its restore
	// tracking survives until an apply succeeds.
	keep := map[client.ObjectKey]bool{}
	var applyErrs []error
	for key, containerPorts := range desiredHostPortKeys(pods.Items) {
		pol := buildHostPortPolicy(ns.Name, key, containerPorts)
		keep[client.ObjectKeyFromObject(pol)] = true
		created, err := applyPolicy(ctx, r.Client, r.Client, pol)
		if err != nil {
			applyFailed(r.Recorder, pol, ApplyErrorHostPort,
				"the host-port NetworkPolicy %s could not be applied: %v. Until this is fixed, traffic to hostPort %d/%s "+
					"on pods in this namespace is blocked.", pol.Name, err, key.port, key.protocol)
			applyErrs = append(applyErrs, fmt.Errorf("apply host-port policy %s: %w", key, err))
			continue
		}
		if r.restores.applied(client.ObjectKeyFromObject(pol), created) {
			policyRestored(r.Recorder, pol,
				"this NetworkPolicy was deleted and has been recreated: kube-vnet lets external traffic reach hostPort %d/%s "+
					"on pods in this namespace through it. An administrator opts the namespace out with the annotation %s=false.",
				key.port, key.protocol, AnnotationExternalAllow)
		}
	}
	r.restores.forgetNamespace(ns.Name, keep)
	// Sweep the policies of (port, protocol) pairs no longer declared.
	sweepErr := sweepStalePolicies(ctx, r.Client, hostPolicies, keep, nil)
	return ctrl.Result{}, errors.Join(append(applyErrs, sweepErr)...)
}

// desiredHostPortKeys returns the distinct (hostPort, protocol) tuples declared
// on any container in any pod in the namespace, each with the sorted,
// deduplicated containerPorts mapped to it. Several containerPorts occur while
// pods of different revisions coexist.
func desiredHostPortKeys(pods []corev1.Pod) map[hostPortKey][]int32 {
	out := map[hostPortKey][]int32{}
	for _, p := range pods {
		if p.Spec.HostNetwork {
			// Out of scope (ADR 0040): most CNIs don't enforce
			// NetworkPolicy on hostNetwork pods.
			continue
		}
		// spec.containers only, as the kubelet's port mappings: a native
		// sidecar's hostPort is reserved by the scheduler but never forwarded.
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.HostPort == 0 {
					continue
				}
				proto := cp.Protocol
				if proto == "" {
					proto = corev1.ProtocolTCP
				}
				key := hostPortKey{port: cp.HostPort, protocol: proto}
				out[key] = append(out[key], cp.ContainerPort)
			}
		}
	}
	for key, ports := range out {
		slices.Sort(ports)
		out[key] = slices.Compact(ports)
	}
	return out
}

// buildHostPortPolicy constructs the policy for one (namespace, hostPort,
// protocol): pods carrying the matching host-port stamp accept the
// containerPorts from `0.0.0.0/0`. The policy is evaluated after the hostPort
// DNAT, so it names the containerPort, not the hostPort.
func buildHostPortPolicy(ns string, key hostPortKey, containerPorts []int32) *networkingv1.NetworkPolicy {
	var ports []networkingv1.NetworkPolicyPort
	for _, p := range containerPorts {
		ports = appendPolicyPort(ports, key.protocol, p)
	}
	return externalAllowPolicy(hostPortPolicyName(ns, key), ns, LabelSourceKindHost,
		fmt.Sprintf("host-%d-%s", key.port, strings.ToLower(string(key.protocol))),
		map[string]string{LabelSystemHostPortPrefix + key.String(): "true"},
		"0.0.0.0/0",
		ports)
}

// hostPortPolicyName returns `kube-vnet.ext.host.<port>.<proto>-<8hex>`
// (ADR 0039/0040).
func hostPortPolicyName(ns string, key hostPortKey) string {
	const prefix = "kube-vnet." + PolicyKindExternal + "." + LabelSourceKindHost + "."
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

// HostPortChangedPredicate fires only when a pod's derived hostPort key to
// containerPorts mapping changes (the policy names the containerPorts), so
// status updates on hostPort pods don't enqueue their namespace.
// Deriving through desiredHostPortKeys also ignores hostNetwork pods.
func HostPortChangedPredicate() predicate.Predicate {
	keysOf := func(obj client.Object) map[hostPortKey][]int32 {
		pod, ok := obj.(*corev1.Pod)
		if !ok || pod == nil {
			return map[hostPortKey][]int32{}
		}
		return desiredHostPortKeys([]corev1.Pod{*pod})
	}
	has := func(obj client.Object) bool { return len(keysOf(obj)) > 0 }

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return has(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return has(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return has(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !maps.EqualFunc(keysOf(e.ObjectOld), keysOf(e.ObjectNew), slices.Equal[[]int32])
		},
	}
}

// objectNamespace maps a namespaced object to a request for its Namespace.
func objectNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
}
