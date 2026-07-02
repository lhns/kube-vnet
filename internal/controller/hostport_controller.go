package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HostPortReconciler emits external-allow NetworkPolicies for pods that
// declare `hostPort` on a container port. Per ADR 0040 — Service-fronted
// exposures (LB/NodePort/ClusterIP+externalIPs) are handled by the
// ExternalAllowReconciler; hostPort is the orthogonal "pod is bound
// directly to a node IP at a specific port" pathway, which NetworkPolicy's
// label-based podSelector can't reference without operator-stamped labels.
//
// Per-(NS, port, protocol) model:
//   - ResolutionReconciler stamps `kube-vnet.system/host-port.<port>.<proto>=true`
//     on every pod declaring that (port, protocol).
//   - HostPortReconciler emits one NetworkPolicy per unique (NS, port, proto)
//     triple seen in the cluster. The policy's podSelector matches the stamp.
//
// Pod identity is ephemeral — Deployment pods are recreated on every
// rollout with new names. Keying policies on (port, protocol) instead of
// on pod name means a new pod inheriting the same hostPort is matched by
// the existing policy automatically; no policy churn on pod replacement.
//
// Same opt-out gates as ExternalAllowReconciler: `kube-vnet/disabled=true`
// or `kube-vnet/external-allow=false` on the Namespace deletes all
// host-port policies in that NS on the next reconcile.
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

	// Same gates as the Service-source path: NS disabled, NS opt-out, etc.
	// (Pod-level external-allow=false isn't a thing — opt-out is NS-wide for
	// hostPort because the policies are per-NS, not per-pod.)
	if !r.NSFilter.IsManaged(ns) || ExternalAllowOptedOut(ns.Annotations) {
		return ctrl.Result{}, r.deleteAllInNamespace(ctx, ns.Name)
	}

	// Collect the desired (port, proto) set by walking every pod in the NS.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns.Name)); err != nil {
		return ctrl.Result{}, err
	}
	desired := desiredHostPortKeys(pods.Items)

	// SSA-apply the desired set.
	for key := range desired {
		pol := buildHostPortPolicy(ns.Name, key)
		pol.SetResourceVersion("")
		if err := r.Patch(ctx, pol, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			logger.Error(err, "apply host-port policy failed", "key", key)
			return ctrl.Result{}, err
		}
	}

	// Self-heal: sweep any host-source external-allow policies in this NS
	// whose (port, protocol) isn't in the desired set. Filter to
	// LabelSourceKind=host so svc-source policies stay untouched.
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

// deleteAllInNamespace removes every operator-emitted host-source
// external-allow policy in the NS. Called when the NS opts out.
// Empty `keep` set passed to the shared sweeper.
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
			// Per ADR 0040 out-of-scope: hostNetwork pods bypass Pod-IP
			// NetworkPolicy enforcement on most CNIs; emitting a policy
			// for them is unreliable.
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

// buildHostPortPolicy constructs the desired NetworkPolicy for one
// (NS, port, protocol) triple. Selects pods stamped with
// `kube-vnet.system/host-port.<port>.<proto>=true` and allows
// `ipBlock: 0.0.0.0/0` on that port.
func buildHostPortPolicy(ns string, key hostPortKey) *networkingv1.NetworkPolicy {
	protoLower := strings.ToLower(string(key.protocol))
	stamp := LabelSystemHostPortPrefix + fmt.Sprintf("%d.%s", key.port, protoLower)
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
				LabelManagedBy:  LabelManagedByValue,
				LabelRole:       LabelRoleExternalAllow,
				LabelSourceKind: LabelSourceKindHost,
				LabelSource:     "host-" + fmt.Sprintf("%d-%s", key.port, protoLower),
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

// hostPortPolicyName returns the deterministic policy name for a
// (NS, port, protocol) triple. Per ADR 0039/0040: the shape is
// `kube-vnet.ext.host.<port>.<proto>-<8hex>`.
func hostPortPolicyName(ns string, key hostPortKey) string {
	const prefix = "kube-vnet." + PolicyKindExternal + "." + PolicySourceKindHostPort + "."
	const hashLen = 8
	identity := key.String() // e.g. "8080.tcp"
	h := sha256.Sum256([]byte(ns + "/" + identity))
	return prefix + identity + "-" + hex.EncodeToString(h[:])[:hashLen]
}

func (r *HostPortReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Pod creates/updates/deletes that touch hostPort declarations need to
	// re-trigger the NS reconcile. Filter to only pods that *currently*
	// declare any hostPort — avoids enqueuing on every pod heartbeat.
	hostPortPodPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return false
		}
		return podHasHostPort(pod)
	})

	// Drift correction: re-enqueue NS when a managed host-source policy
	// changes (delete/edit). Filter by LabelSourceKind=host so a Service
	// literally named `host-…` doesn't get this reconciler involved.
	hostPolicyPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue &&
			l[LabelRole] == LabelRoleExternalAllow &&
			l[LabelSourceKind] == LabelSourceKindHost
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("host-port").
		For(&corev1.Namespace{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(podToNamespace),
			builder.WithPredicates(hostPortPodPredicate),
		).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(networkPolicyToNamespace),
			builder.WithPredicates(hostPolicyPredicate),
		).
		Complete(r)
}

func podHasHostPort(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.HostPort != 0 {
				return true
			}
		}
	}
	return false
}

func podToNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
}

func networkPolicyToNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
}

