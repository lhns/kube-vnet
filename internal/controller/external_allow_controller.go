package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ExternalAllowReconciler emits "allow-from-anywhere" NetworkPolicies for
// externally-exposed Services (LoadBalancer, NodePort, or ClusterIP with
// externalIPs set). Per ADR 0038 — the gap NetworkPolicy can't naturally
// close, because external-traffic source IPs (node-SNAT after kube-proxy
// or the original client IP) never match any namespaceSelector.
//
// The policy is additive (NetworkPolicy union), so vnet isolation is
// unchanged; only the exposed targetPorts open to `0.0.0.0/0`.
//
// Default-on. Opt out with `kube-vnet/external-allow=false` on the Service or
// its Namespace.
type ExternalAllowReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
	Recorder events.EventRecorder

	restores policyTracker
	pending  onceSet
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *ExternalAllowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return svcSourcePolicies.reconcile(ctx, req,
		serviceReconciler{r.Client, r.Scheme, r.NSFilter, r.Recorder, &r.restores, &r.pending}, r.desired)
}

// desired returns the Service's policy, or nil if it isn't externally exposed
// or has no selector to mirror.
func (r *ExternalAllowReconciler) desired(ctx context.Context, svc *corev1.Service) (*networkingv1.NetworkPolicy, error) {
	// Pods resolve named targetPorts.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(svc.Namespace)); err != nil {
		return nil, err
	}
	desired, err := buildExternalAllowPolicy(svc, pods.Items)
	// An exposed Service without a selector (manually-managed Endpoints) gets
	// an Event, since its missing policy is otherwise unexplained.
	if desired == nil && err == nil && isExternallyExposed(svc) && len(svc.Spec.Selector) == 0 {
		eventf(r.Recorder, svc, corev1.EventTypeWarning, ReasonServiceHasNoSelector, "Reconcile",
			"the Service is exposed externally but has no spec.selector, so kube-vnet cannot tell which pods to "+
				"open to external clients; they stay blocked by the namespace baseline. Add a selector, or write "+
				"a NetworkPolicy for the pods behind the Service's Endpoints.")
	}
	return desired, err
}

// buildExternalAllowPolicy returns the policy for an externally exposed
// Service, nil if the Service isn't exposed or has no selector, or
// errNamedPortUnresolvable if a named targetPort has no backing pod yet. One
// unresolvable port fails the whole Service rather than emitting a partial
// policy.
func buildExternalAllowPolicy(svc *corev1.Service, podsInNS []corev1.Pod) (*networkingv1.NetworkPolicy, error) {
	if !isExternallyExposed(svc) || len(svc.Spec.Selector) == 0 {
		return nil, nil
	}

	var ports []networkingv1.NetworkPolicyPort
	for _, sp := range svc.Spec.Ports {
		targetPorts, err := resolveTargetPorts(sp, svc.Spec.Selector, podsInNS)
		if err != nil {
			return nil, err
		}
		proto := sp.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		for _, tp := range targetPorts {
			ports = appendPolicyPort(ports, proto, tp)
		}
	}
	return svcSourcePolicies.policy(svc, externalAllowPolicyName(svc), "0.0.0.0/0", ports), nil
}

// isExternallyExposed returns true if the Service shape carries external
// traffic to its backing pods:
//   - type=LoadBalancer
//   - type=NodePort
//   - type=ClusterIP AND spec.externalIPs is non-empty
//
// Headless Services (clusterIP=None) and ExternalName Services never carry
// external traffic to backing pods.
func isExternallyExposed(svc *corev1.Service) bool {
	if svc == nil {
		return false
	}
	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return false
	}
	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort:
		return true
	case corev1.ServiceTypeClusterIP, "":
		// "" defaults to ClusterIP.
		return len(svc.Spec.ExternalIPs) > 0
	}
	return false
}

// externalAllowPolicyName returns the Service-source policy name,
// `kube-vnet.ext.svc.<svcName>-<8hex>` (ADR 0039).
func externalAllowPolicyName(svc *corev1.Service) string {
	return servicePolicyName(LabelSourceKindService, svc.Namespace, svc.Name)
}

func (r *ExternalAllowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("external-allow").
		For(&corev1.Service{})
	return svcSourcePolicies.watchInputs(b, mgr, r.Client).Complete(r)
}
