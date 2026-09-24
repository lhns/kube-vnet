package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	applied := false
	defer apiserverPolicies.forgetUnless(&applied, &r.restores, req.Namespace, req.Name)
	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, apiserverPolicies.deleteByServiceKey(ctx, r.Client, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// NamespaceLifecycle admission rejects creates in a terminating namespace.
	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Same opt-out gates as ExternalAllowReconciler.
	if !r.NSFilter.IsManaged(ns) ||
		ExternalAllowOptedOut(ns.Annotations) ||
		ExternalAllowOptedOut(svc.Annotations) {
		return ctrl.Result{}, apiserverPolicies.sweep(ctx, r.Client, svc, "")
	}

	// Headless / ExternalName / selector-less Services have no podSelector
	// to mirror.
	if svc.Spec.ClusterIP == corev1.ClusterIPNone || svc.Spec.Type == corev1.ServiceTypeExternalName ||
		len(svc.Spec.Selector) == 0 {
		return ctrl.Result{}, apiserverPolicies.sweep(ctx, r.Client, svc, "")
	}

	ports, err := r.collectReferencedPorts(ctx, svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(ports) == 0 {
		// Nothing references this Service and it hasn't opted in.
		return ctrl.Result{}, apiserverPolicies.sweep(ctx, r.Client, svc, "")
	}

	// Pods resolve named targetPorts.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(svc.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	desired, err := buildApiserverReachablePolicy(svc, pods.Items, ports, r.SourceCIDR)
	if err != nil {
		if errors.Is(err, errNamedPortUnresolvable) {
			r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, "Pending", "Reconcile",
				"apiserver-reachable policy pending: a named targetPort has no backing pod with the matching containerPort name")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	applied, err = apiserverPolicies.applyAndReport(ctx, r.Client, r.Scheme, r.Recorder, &r.restores, svc, desired)
	return ctrl.Result{}, err
}

// collectReferencedPorts walks all four discovery resource kinds and the
// Service's own annotation, returning the sorted unique set of ports
// reached for this Service. Returns nil if nothing references this Service.
func (r *ApiserverReachableReconciler) collectReferencedPorts(ctx context.Context, svc *corev1.Service) ([]int32, error) {
	var refs []serviceRef

	var vwhcs admissionregistrationv1.ValidatingWebhookConfigurationList
	if err := r.List(ctx, &vwhcs); err != nil {
		return nil, err
	}
	for i := range vwhcs.Items {
		refs = append(refs, extractValidatingWebhookRefs(&vwhcs.Items[i])...)
	}

	var mwhcs admissionregistrationv1.MutatingWebhookConfigurationList
	if err := r.List(ctx, &mwhcs); err != nil {
		return nil, err
	}
	for i := range mwhcs.Items {
		refs = append(refs, extractMutatingWebhookRefs(&mwhcs.Items[i])...)
	}

	var apisvcs apiregistrationv1.APIServiceList
	if err := r.List(ctx, &apisvcs); err != nil {
		return nil, err
	}
	for i := range apisvcs.Items {
		refs = append(refs, extractAPIServiceRefs(&apisvcs.Items[i])...)
	}

	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := r.List(ctx, &crds); err != nil {
		return nil, err
	}
	for i := range crds.Items {
		refs = append(refs, extractCRDConversionRefs(&crds.Items[i])...)
	}

	portSet := map[int32]struct{}{}
	for _, ref := range refs {
		if ref.Namespace == svc.Namespace && ref.Name == svc.Name {
			portSet[ref.Port] = struct{}{}
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

// extractValidatingWebhookRefs returns the Service refs declared in a
// ValidatingWebhookConfiguration. URL-only entries are out-of-cluster and
// skipped.
func extractValidatingWebhookRefs(cfg *admissionregistrationv1.ValidatingWebhookConfiguration) []serviceRef {
	if cfg == nil {
		return nil
	}
	out := make([]serviceRef, 0, len(cfg.Webhooks))
	for _, wh := range cfg.Webhooks {
		if wh.ClientConfig.Service == nil {
			continue
		}
		s := wh.ClientConfig.Service
		out = append(out, newServiceRef(s.Namespace, s.Name, s.Port))
	}
	return out
}

// extractMutatingWebhookRefs is extractValidatingWebhookRefs for
// MutatingWebhookConfiguration.
func extractMutatingWebhookRefs(cfg *admissionregistrationv1.MutatingWebhookConfiguration) []serviceRef {
	if cfg == nil {
		return nil
	}
	out := make([]serviceRef, 0, len(cfg.Webhooks))
	for _, wh := range cfg.Webhooks {
		if wh.ClientConfig.Service == nil {
			continue
		}
		s := wh.ClientConfig.Service
		out = append(out, newServiceRef(s.Namespace, s.Name, s.Port))
	}
	return out
}

// extractAPIServiceRefs returns the Service ref declared in an APIService.
// Local APIServices (`spec.service: nil`) are served by the apiserver itself.
func extractAPIServiceRefs(api *apiregistrationv1.APIService) []serviceRef {
	if api == nil || api.Spec.Service == nil {
		return nil
	}
	s := api.Spec.Service
	return []serviceRef{newServiceRef(s.Namespace, s.Name, s.Port)}
}

// extractCRDConversionRefs returns the Service ref of a CRD's conversion
// webhook, if it has one.
func extractCRDConversionRefs(crd *apiextensionsv1.CustomResourceDefinition) []serviceRef {
	if crd == nil || crd.Spec.Conversion == nil {
		return nil
	}
	conv := crd.Spec.Conversion
	if conv.Strategy != apiextensionsv1.WebhookConverter {
		return nil
	}
	if conv.Webhook == nil || conv.Webhook.ClientConfig == nil || conv.Webhook.ClientConfig.Service == nil {
		return nil
	}
	s := conv.Webhook.ClientConfig.Service
	return []serviceRef{newServiceRef(s.Namespace, s.Name, s.Port)}
}

// newServiceRef builds a serviceRef, defaulting an unset port to 443 per the
// Kubernetes API spec.
func newServiceRef(namespace, name string, port *int32) serviceRef {
	p := int32(443)
	if port != nil && *port != 0 {
		p = *port
	}
	return serviceRef{Namespace: namespace, Name: name, Port: p}
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
	return servicePolicyName(LabelSourceKindApiserver, svc)
}

func (r *ApiserverReachableReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("apiserver-reachable").
		For(&corev1.Service{})
	return apiserverPolicies.watchInputs(b, mgr, r.Client).
		Watches(
			&admissionregistrationv1.ValidatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(validatingWebhookToServices),
		).
		Watches(
			&admissionregistrationv1.MutatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(mutatingWebhookToServices),
		).
		Watches(
			&apiregistrationv1.APIService{},
			handler.EnqueueRequestsFromMapFunc(apiServiceToServices),
		).
		Watches(
			&apiextensionsv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(crdConversionToServices),
		).
		Complete(r)
}

// validatingWebhookToServices / mutatingWebhookToServices / apiServiceToServices /
// crdConversionToServices map an event on a cluster-scoped discovery resource
// to requests for the Services it references.

func validatingWebhookToServices(_ context.Context, obj client.Object) []reconcile.Request {
	cfg, ok := obj.(*admissionregistrationv1.ValidatingWebhookConfiguration)
	if !ok {
		return nil
	}
	return refsToRequests(extractValidatingWebhookRefs(cfg))
}

func mutatingWebhookToServices(_ context.Context, obj client.Object) []reconcile.Request {
	cfg, ok := obj.(*admissionregistrationv1.MutatingWebhookConfiguration)
	if !ok {
		return nil
	}
	return refsToRequests(extractMutatingWebhookRefs(cfg))
}

func apiServiceToServices(_ context.Context, obj client.Object) []reconcile.Request {
	api, ok := obj.(*apiregistrationv1.APIService)
	if !ok {
		return nil
	}
	return refsToRequests(extractAPIServiceRefs(api))
}

func crdConversionToServices(_ context.Context, obj client.Object) []reconcile.Request {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return nil
	}
	return refsToRequests(extractCRDConversionRefs(crd))
}

// refsToRequests dedupes refs to one request per Service.
func refsToRequests(refs []serviceRef) []reconcile.Request {
	seen := map[types.NamespacedName]struct{}{}
	out := make([]reconcile.Request, 0, len(refs))
	for _, ref := range refs {
		key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, reconcile.Request{NamespacedName: key})
	}
	return out
}
