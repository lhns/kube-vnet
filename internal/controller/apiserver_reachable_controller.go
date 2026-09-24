package controller

import (
	"context"
	"maps"
	"slices"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ApiserverReachableReconciler emits "allow-from-anywhere" NetworkPolicies
// for Services that the kube-apiserver reaches in-cluster. Per ADR 0041 —
// the gap NetworkPolicy can't naturally close, because the apiserver
// isn't a pod and its source IP (control-plane node IP or managed-control-
// plane IP) doesn't match any namespaceSelector or podSelector.
//
// Trigger surface: four cluster-scoped Kubernetes resources that declare
// "the apiserver dials this Service," plus an opt-in annotation on Services
// for cases the four don't cover.
//
//	ValidatingWebhookConfiguration  webhooks[].clientConfig.service
//	MutatingWebhookConfiguration    webhooks[].clientConfig.service
//	APIService                      spec.service
//	CustomResourceDefinition        spec.conversion.webhook.clientConfig.service
//	corev1.Service                  annotation kube-vnet/apiserver-reachable=true
//
// The policy is additive (NetworkPolicy union), so vnet isolation is
// unchanged; the apiserver only gains a path to the webhook's targetPort.
//
// Default-on. Opt out with `kube-vnet/external-allow=false` on the Service or
// its Namespace, the same annotation as ADR 0038.
type ApiserverReachableReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
	Recorder events.EventRecorder

	// SourceCIDR is the emitted policy's `from: ipBlock`. Empty means
	// `0.0.0.0/0`; admins can narrow it to their control-plane subnet.
	SourceCIDR string

	restores policyTracker
	pending  onceSet
}

// serviceRef identifies a Service port the apiserver reaches. Refs are not
// unique: several webhooks can name the same Service and port.
type serviceRef struct {
	Namespace string
	Name      string
	Port      int32
}

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiregistration.k8s.io,resources=apiservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *ApiserverReachableReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return apiserverPolicies.reconcile(ctx, req,
		serviceReconciler{r.Client, r.Scheme, r.NSFilter, r.Recorder, &r.restores, &r.pending}, r.desired)
}

// desired returns the Service's policy, or nil if nothing the apiserver dials
// names it and it hasn't opted in.
func (r *ApiserverReachableReconciler) desired(ctx context.Context, svc *corev1.Service) (*networkingv1.NetworkPolicy, error) {
	// Headless / ExternalName / selector-less Services have no podSelector
	// to mirror.
	if svc.Spec.ClusterIP == corev1.ClusterIPNone || svc.Spec.Type == corev1.ServiceTypeExternalName ||
		len(svc.Spec.Selector) == 0 {
		return nil, nil
	}
	ports, err := r.collectReferencedPorts(ctx, svc)
	if err != nil || len(ports) == 0 {
		return nil, err
	}
	// Pods resolve named targetPorts.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(svc.Namespace)); err != nil {
		return nil, err
	}
	return buildApiserverReachablePolicy(svc, pods.Items, ports, r.SourceCIDR)
}

// collectReferencedPorts returns the sorted unique ports the discovery
// resources and the Service's own annotation reach it on; nil if none.
func (r *ApiserverReachableReconciler) collectReferencedPorts(ctx context.Context, svc *corev1.Service) ([]int32, error) {
	portSet := map[int32]struct{}{}
	for _, list := range []client.ObjectList{
		&admissionregistrationv1.ValidatingWebhookConfigurationList{},
		&admissionregistrationv1.MutatingWebhookConfigurationList{},
		&apiregistrationv1.APIServiceList{},
		&apiextensionsv1.CustomResourceDefinitionList{},
	} {
		if err := r.List(ctx, list); err != nil {
			return nil, err
		}
		if err := meta.EachListItem(list, func(obj runtime.Object) error {
			for _, ref := range serviceRefs(obj) {
				if ref.Namespace == svc.Namespace && ref.Name == svc.Name {
					portSet[ref.Port] = struct{}{}
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	// The opt-in annotation covers every Service port; per-port control
	// means writing the NetworkPolicy by hand.
	if ApiserverReachableOptedIn(svc.Annotations) {
		for _, sp := range svc.Spec.Ports {
			portSet[sp.Port] = struct{}{}
		}
	}

	return slices.Sorted(maps.Keys(portSet)), nil
}

// serviceRefs returns the Service refs a discovery resource declares, one per
// entry. URL-only entries are out of cluster and skipped, as are local
// APIServices (`spec.service: nil`, served by the apiserver itself). An unset
// port defaults to 443, per the Kubernetes API.
func serviceRefs(obj runtime.Object) []serviceRef {
	var out []serviceRef
	add := func(namespace, name string, port *int32) {
		p := int32(443)
		if port != nil && *port != 0 {
			p = *port
		}
		out = append(out, serviceRef{Namespace: namespace, Name: name, Port: p})
	}
	switch o := obj.(type) {
	case *admissionregistrationv1.ValidatingWebhookConfiguration:
		for _, wh := range o.Webhooks {
			if s := wh.ClientConfig.Service; s != nil {
				add(s.Namespace, s.Name, s.Port)
			}
		}
	case *admissionregistrationv1.MutatingWebhookConfiguration:
		for _, wh := range o.Webhooks {
			if s := wh.ClientConfig.Service; s != nil {
				add(s.Namespace, s.Name, s.Port)
			}
		}
	case *apiregistrationv1.APIService:
		if s := o.Spec.Service; s != nil {
			add(s.Namespace, s.Name, s.Port)
		}
	case *apiextensionsv1.CustomResourceDefinition:
		conv := o.Spec.Conversion
		if conv != nil && conv.Strategy == apiextensionsv1.WebhookConverter &&
			conv.Webhook != nil && conv.Webhook.ClientConfig != nil && conv.Webhook.ClientConfig.Service != nil {
			s := conv.Webhook.ClientConfig.Service
			add(s.Namespace, s.Name, s.Port)
		}
	}
	return out
}

// buildApiserverReachablePolicy constructs the desired NetworkPolicy for a
// Service reached on the given ports, which must be non-empty and sorted so
// the output is stable.
//
// NetworkPolicy is enforced after kube-proxy DNATs Service:port to
// pod:targetPort, so the policy must allow the pod-side port, including
// named targetPorts resolved from the backing pods. If any is unresolvable it
// returns errNamedPortUnresolvable rather than a partial policy, as ADR 0038
// does.
//
// A discovery port the Service doesn't declare (webhook config out of sync
// with the Service) is emitted as-is rather than dropped.
func buildApiserverReachablePolicy(svc *corev1.Service, podsInNS []corev1.Pod, ports []int32, sourceCIDR string) (*networkingv1.NetworkPolicy, error) {
	if sourceCIDR == "" {
		sourceCIDR = "0.0.0.0/0"
	}
	var policyPorts []networkingv1.NetworkPolicyPort
	for _, port := range ports {
		targetPorts := []int32{port}
		if sp, ok := findServicePort(svc, port); ok {
			var err error
			if targetPorts, err = resolveTargetPorts(sp, svc.Spec.Selector, podsInNS); err != nil {
				return nil, err
			}
		}
		for _, tp := range targetPorts {
			// The apiserver always dials HTTPS over TCP.
			policyPorts = appendPolicyPort(policyPorts, corev1.ProtocolTCP, tp)
		}
	}
	return apiserverPolicies.policy(svc, apiserverReachablePolicyName(svc), sourceCIDR, policyPorts), nil
}

// findServicePort returns the spec.ports entry with the given Port.
func findServicePort(svc *corev1.Service, port int32) (corev1.ServicePort, bool) {
	for _, sp := range svc.Spec.Ports {
		if sp.Port == port {
			return sp, true
		}
	}
	return corev1.ServicePort{}, false
}

// apiserverReachablePolicyName returns
// `kube-vnet.ext.apiserver.<svcName>-<8hex>` (ADR 0039).
func apiserverReachablePolicyName(svc *corev1.Service) string {
	return servicePolicyName(LabelSourceKindApiserver, svc.Namespace, svc.Name)
}

func (r *ApiserverReachableReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := apiserverPolicies.watchInputs(ctrl.NewControllerManagedBy(mgr).
		Named("apiserver-reachable").
		For(&corev1.Service{}), mgr, r.Client)
	for _, obj := range []client.Object{
		&admissionregistrationv1.ValidatingWebhookConfiguration{},
		&admissionregistrationv1.MutatingWebhookConfiguration{},
		&apiregistrationv1.APIService{},
		&apiextensionsv1.CustomResourceDefinition{},
	} {
		b = b.Watches(obj, handler.EnqueueRequestsFromMapFunc(discoveryToServices))
	}
	return b.Complete(r)
}

// discoveryToServices maps an event on a discovery resource to one request per
// Service it references.
func discoveryToServices(_ context.Context, obj client.Object) []reconcile.Request {
	var out []reconcile.Request
	for _, ref := range serviceRefs(obj) {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
		if !slices.Contains(out, req) {
			out = append(out, req)
		}
	}
	return out
}
