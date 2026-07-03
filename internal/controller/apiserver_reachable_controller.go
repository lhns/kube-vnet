package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
// The emitted policy is additive: it shares the namespace with kube-vnet's
// baseline + membership policies and the ADR 0038 / 0040 external-allow
// policies, composing via NetworkPolicy union semantics. Pod-to-pod
// isolation through vnet membership keeps working; the apiserver gets a
// narrow path to the webhook's targetPort.
//
// Default-on. Opt-out via the same `kube-vnet/external-allow=false`
// annotation ADR 0038 uses, on the Service or on its Namespace — single
// vocabulary across the auto-allow family.
type ApiserverReachableReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NSFilter *NamespaceFilter
	Recorder events.EventRecorder

	// SourceCIDR is the CIDR the emitted policy's `from: ipBlock` uses.
	// Default `0.0.0.0/0` matches the cluster's no-NetworkPolicy baseline
	// (the same posture pods have today before kube-vnet is installed).
	// Admins set this to their control-plane subnet for tighter narrowing.
	SourceCIDR string
}

// serviceRef identifies a single Service + port the apiserver reaches.
// Multiple discovery resources can produce the same ref (deduped at the
// reconciler); a single discovery resource can produce multiple refs (e.g.
// a ValidatingWHC with N webhooks on different ports of the same Service).
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
	logger := log.FromContext(ctx).WithValues("service", req.NamespacedName)

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			// Service is gone. The apiserver-GC cascade on the Service
			// owner-reference handles the policy delete in a real cluster,
			// but envtest doesn't run the GC controller — explicit
			// label-based delete handles that case + any pre-existing
			// policy from before this code shipped.
			return ctrl.Result{}, r.deletePolicyByServiceKey(ctx, req.Namespace, req.Name)
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

	// Three opt-out gates, same shape as ExternalAllowReconciler: NS in
	// --disabled-namespaces or annotated kube-vnet/disabled=true; NS or
	// Service annotated kube-vnet/external-allow=false. Single annotation
	// across the auto-allow family so users don't learn two opt-outs.
	if !r.NSFilter.IsManaged(ns) ||
		ExternalAllowOptedOut(ns.Annotations) ||
		ExternalAllowOptedOut(svc.Annotations) {
		return ctrl.Result{}, r.deletePolicyForService(ctx, svc)
	}

	// Headless / ExternalName / selector-less Services have no podSelector
	// for us to mirror. Skip cleanly.
	if svc.Spec.ClusterIP == corev1.ClusterIPNone || len(svc.Spec.Selector) == 0 {
		return ctrl.Result{}, r.deletePolicyForService(ctx, svc)
	}

	ports, err := r.collectReferencedPorts(ctx, svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(ports) == 0 {
		// No discovery resource references this Service, and no annotation
		// opts it in. Clean up any stale policy.
		return ctrl.Result{}, r.deletePolicyForService(ctx, svc)
	}

	// Named targetPorts (e.g. `targetPort: https` in cert-manager-webhook's
	// Service) need pod-side resolution: walk pods matching the Service
	// selector to find the containerPort whose name matches. Same machinery
	// as ADR 0038 — `resolveTargetPort` lives in external_allow_controller.go.
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

	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply apiserver-reachable policy failed")
		return ctrl.Result{}, err
	}

	// Self-heal: sweep stale apiserver-reachable policies for THIS Service.
	keep := map[client.ObjectKey]bool{
		{Namespace: svc.Namespace, Name: desired.Name}: true,
	}
	if err := sweepStalePoliciesByOwner(ctx, r.Client,
		inNamespacePolicyLabels(svc.Namespace, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: LabelSourceKindApiserver,
		}),
		"Service", svc.Name, svc.UID,
		keep,
		nil, // List filter already narrows to source-kind=apiserver
	); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// collectReferencedPorts walks all four discovery resource kinds and the
// Service's own annotation, returning the sorted unique set of ports
// reached for this Service. Returns nil if nothing references this Service.
func (r *ApiserverReachableReconciler) collectReferencedPorts(ctx context.Context, svc *corev1.Service) ([]int32, error) {
	portSet := map[int32]struct{}{}

	// 1. ValidatingWebhookConfigurations
	var vwhcs admissionregistrationv1.ValidatingWebhookConfigurationList
	if err := r.List(ctx, &vwhcs); err != nil {
		return nil, err
	}
	for i := range vwhcs.Items {
		for _, ref := range extractValidatingWebhookRefs(&vwhcs.Items[i]) {
			if ref.Namespace == svc.Namespace && ref.Name == svc.Name {
				portSet[ref.Port] = struct{}{}
			}
		}
	}

	// 2. MutatingWebhookConfigurations
	var mwhcs admissionregistrationv1.MutatingWebhookConfigurationList
	if err := r.List(ctx, &mwhcs); err != nil {
		return nil, err
	}
	for i := range mwhcs.Items {
		for _, ref := range extractMutatingWebhookRefs(&mwhcs.Items[i]) {
			if ref.Namespace == svc.Namespace && ref.Name == svc.Name {
				portSet[ref.Port] = struct{}{}
			}
		}
	}

	// 3. APIServices
	var apisvcs apiregistrationv1.APIServiceList
	if err := r.List(ctx, &apisvcs); err != nil {
		return nil, err
	}
	for i := range apisvcs.Items {
		for _, ref := range extractAPIServiceRefs(&apisvcs.Items[i]) {
			if ref.Namespace == svc.Namespace && ref.Name == svc.Name {
				portSet[ref.Port] = struct{}{}
			}
		}
	}

	// 4. CustomResourceDefinitions (conversion webhook)
	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := r.List(ctx, &crds); err != nil {
		return nil, err
	}
	for i := range crds.Items {
		for _, ref := range extractCRDConversionRefs(&crds.Items[i]) {
			if ref.Namespace == svc.Namespace && ref.Name == svc.Name {
				portSet[ref.Port] = struct{}{}
			}
		}
	}

	// 5. Annotation escape hatch — Service opts itself in for every
	// Service port. Treats the annotation as "expose all my ports" rather
	// than per-port; users who want per-port use the existing single-
	// Service hand-written NetworkPolicy escape path.
	if ApiserverReachableOptedIn(svc.Annotations) {
		for _, sp := range svc.Spec.Ports {
			portSet[sp.Port] = struct{}{}
		}
	}

	if len(portSet) == 0 {
		return nil, nil
	}
	ports := make([]int32, 0, len(portSet))
	for p := range portSet {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports, nil
}

// extractValidatingWebhookRefs returns the Service refs declared in a
// ValidatingWebhookConfiguration. URL-only `clientConfig.url` entries are
// skipped — they're out-of-cluster and don't need NetworkPolicy emission.
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
		out = append(out, serviceRef{
			Namespace: s.Namespace,
			Name:      s.Name,
			Port:      derefPortOr443(s.Port),
		})
	}
	return out
}

// extractMutatingWebhookRefs is the symmetric extractor for
// MutatingWebhookConfiguration. Same field shape; different Go type.
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
		out = append(out, serviceRef{
			Namespace: s.Namespace,
			Name:      s.Name,
			Port:      derefPortOr443(s.Port),
		})
	}
	return out
}

// extractAPIServiceRefs returns the Service ref declared in an APIService.
// `spec.service: nil` means a local APIService (the apiserver itself hosts
// the API; no aggregation target) — skipped.
func extractAPIServiceRefs(api *apiregistrationv1.APIService) []serviceRef {
	if api == nil || api.Spec.Service == nil {
		return nil
	}
	s := api.Spec.Service
	return []serviceRef{{
		Namespace: s.Namespace,
		Name:      s.Name,
		Port:      derefPortOr443(s.Port),
	}}
}

// extractCRDConversionRefs returns the Service ref declared in a CRD's
// conversion-webhook config. Skipped if conversion isn't configured or
// uses strategy `None` or `webhook` but with URL-only clientConfig.
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
	return []serviceRef{{
		Namespace: s.Namespace,
		Name:      s.Name,
		Port:      derefPortOr443(s.Port),
	}}
}

// derefPortOr443 returns the int32 port from a *int32 pointer (admission-
// registration and APIService all use *int32 here), defaulting to 443 per
// the Kubernetes API spec when unset.
func derefPortOr443(p *int32) int32 {
	if p == nil || *p == 0 {
		return 443
	}
	return *p
}

// buildApiserverReachablePolicy constructs the desired NetworkPolicy for a
// Service whose discovery resources reference the given ports. `ports`
// must be non-empty and pre-sorted; ingress.ports[i] mirrors that order
// for stable diffs.
//
// Why pods are an input: NetworkPolicy is enforced after kube-proxy DNATs
// the apiserver's `Service:port` connection to `pod:targetPort`. The
// kernel sees the POD-side port. So we MUST emit the resolved pod-side
// targetPort, not the Service-side port — including resolving the
// `targetPort: <name>` (named string) form to its containerPort number
// by walking the backing pods. Reuses `resolveTargetPort` from
// external_allow_controller.go, which has handled this since ADR 0038.
//
// Returns errNamedPortUnresolvable if any referenced Service port maps
// to a named targetPort whose backing pod isn't running yet (or doesn't
// declare a matching containerPort name). Caller treats this as a
// transient state — emits a Pending Event and requeues. Multi-port
// Services with one unresolvable port return the error rather than
// partial emission, matching ADR 0038's all-or-nothing behavior.
//
// If the Service spec doesn't declare the discovery-referenced port at
// all (rare: webhook config out of sync with the Service), the policy
// emits the discovery port directly as a safe passthrough. Avoids
// dropping admission silently while the user reconciles their config.
func buildApiserverReachablePolicy(svc *corev1.Service, podsInNS []corev1.Pod, ports []int32, sourceCIDR string) (*networkingv1.NetworkPolicy, error) {
	if sourceCIDR == "" {
		sourceCIDR = "0.0.0.0/0"
	}
	policyPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		var targetPort int32
		sp, ok := findServicePort(svc, port)
		if ok {
			tp, err := resolveTargetPort(sp, svc.Spec.Selector, podsInNS)
			if err != nil {
				return nil, err
			}
			targetPort = tp
		} else {
			// Service spec doesn't declare the discovery port at all —
			// pass it through. kube-proxy will fail to DNAT this
			// connection anyway, but emitting the policy is the
			// right thing to do (drops a `kubectl describe svc`
			// breadcrumb for the user).
			targetPort = port
		}

		// Default protocol: TCP. Webhook + APIService traffic is always
		// HTTPS over TCP per spec.
		proto := corev1.ProtocolTCP
		policyPorts = append(policyPorts, networkingv1.NetworkPolicyPort{
			Protocol: &proto,
			Port:     ptrIntOrString(intstr.FromInt32(targetPort)),
		})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiserverReachablePolicyName(svc),
			Namespace: svc.Namespace,
			Labels: map[string]string{
				LabelManagedBy:    LabelManagedByValue,
				LabelK8sManagedBy: LabelManagedByValue,
				LabelRole:         LabelRoleExternalAllow,
				LabelSourceKind:   LabelSourceKindApiserver,
				LabelSource:       "apiserver-" + svc.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: cloneStringMap(svc.Spec.Selector),
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: sourceCIDR}},
					},
					Ports: policyPorts,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}, nil
}

// findServicePort returns the spec.ports entry whose Port matches the
// discovery-referenced port. The second value is false if no entry
// declares that port (webhook config out of sync with the Service).
func findServicePort(svc *corev1.Service, port int32) (corev1.ServicePort, bool) {
	for _, sp := range svc.Spec.Ports {
		if sp.Port == port {
			return sp, true
		}
	}
	return corev1.ServicePort{}, false
}

func ptrIntOrString(v intstr.IntOrString) *intstr.IntOrString {
	return &v
}

// apiserverReachablePolicyName returns a deterministic policy name per
// ADR 0039: `kube-vnet.ext.apiserver.<svcName>-<8hex>`, total ≤63 chars.
// The hash is over <ns>/<name>; cross-NS collisions on a truncated base
// are made unique by NS in the hash.
func apiserverReachablePolicyName(svc *corev1.Service) string {
	const prefix = "kube-vnet." + PolicyKindExternal + "." + PolicySourceKindApiserver + "."
	const hashLen = 8
	const maxNameLen = 63
	maxBase := maxNameLen - len(prefix) - 1 - hashLen
	base := svc.Name
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	h := sha256.Sum256([]byte(svc.Namespace + "/" + svc.Name))
	return prefix + base + "-" + hex.EncodeToString(h[:])[:hashLen]
}

// deletePolicyForService removes every apiserver-reachable policy owned
// by this Service. Mirrors ExternalAllowReconciler.deletePolicyForService.
func (r *ApiserverReachableReconciler) deletePolicyForService(ctx context.Context, svc *corev1.Service) error {
	return sweepStalePoliciesByOwner(ctx, r.Client,
		inNamespacePolicyLabels(svc.Namespace, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: LabelSourceKindApiserver,
		}),
		"Service", svc.Name, svc.UID,
		nil, // keep nothing
		nil, // List filter already narrows to source-kind=apiserver
	)
}

// deletePolicyByServiceKey handles the Service-NotFound path. Without the
// Service object we can't owner-ref-match, but the LabelSource is stable
// at `apiserver-<svcName>` for the current format. Apiserver GC handles
// owner-ref-bound policies separately when the Service is deleted in a
// real cluster.
func (r *ApiserverReachableReconciler) deletePolicyByServiceKey(ctx context.Context, namespace, serviceName string) error {
	return sweepStalePolicies(ctx, r.Client,
		inNamespacePolicyLabels(namespace, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: LabelSourceKindApiserver,
			LabelSource:     "apiserver-" + serviceName,
		}),
		nil,
	)
}

func (r *ApiserverReachableReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Re-enqueue the source Service when our apiserver-reachable policy
	// is touched (delete = drift correction; user-edit-then-update =
	// SSA reapply). Filter by the source-kind label so this watch doesn't
	// fight with the ExternalAllowReconciler's policy watch.
	apiserverPolPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue &&
			l[LabelRole] == LabelRoleExternalAllow &&
			l[LabelSourceKind] == LabelSourceKindApiserver
	})

	// Pod creates only — a new pod is the only event that can unblock a
	// previously-unresolvable named targetPort. Mirrors the same predicate
	// in ExternalAllowReconciler (ADR 0038).
	podCreateOnly := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("apiserver-reachable").
		For(&corev1.Service{}).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(apiserverReachablePolicyToService),
			builder.WithPredicates(apiserverPolPredicate),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToServices),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToServicesWithNamedPorts),
			builder.WithPredicates(podCreateOnly),
		).
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

// podToServicesWithNamedPorts enqueues every Service in the new pod's
// namespace that uses any named (string-typed) targetPort — those are
// the only Services whose policy emission might have been blocked
// waiting for this pod's containerPort names. Numeric-targetPort
// Services don't depend on Pod state at all. Bounded by NS size.
//
// Mirrors ExternalAllowReconciler.podToServicesWithNamedPorts; reuses
// the same `hasNamedTargetPort` helper from external_allow_controller.go.
func (r *ApiserverReachableReconciler) podToServicesWithNamedPorts(ctx context.Context, obj client.Object) []reconcile.Request {
	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0)
	for i := range svcs.Items {
		if !hasNamedTargetPort(&svcs.Items[i]) {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: svcs.Items[i].Namespace,
				Name:      svcs.Items[i].Name,
			},
		})
	}
	return out
}

// apiserverReachablePolicyToService derives the source Service from the
// `kube-vnet.system/source: apiserver-<name>` label.
func apiserverReachablePolicyToService(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[LabelSourceKind] != LabelSourceKindApiserver {
		return nil
	}
	const prefix = "apiserver-"
	src := l[LabelSource]
	if !strings.HasPrefix(src, prefix) {
		return nil
	}
	name := strings.TrimPrefix(src, prefix)
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// namespaceToServices enqueues every Service in the namespace that
// changed. Catches mid-flight kube-vnet/external-allow=false flips
// where no Service event fires but existing policies should go away.
func (r *ApiserverReachableReconciler) namespaceToServices(ctx context.Context, obj client.Object) []reconcile.Request {
	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, client.InNamespace(obj.GetName())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(svcs.Items))
	for i := range svcs.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: svcs.Items[i].Namespace,
				Name:      svcs.Items[i].Name,
			},
		})
	}
	return out
}

// validatingWebhookToServices / mutatingWebhookToServices / apiServiceToServices /
// crdConversionToServices map an event on a cluster-scoped discovery resource
// to reconcile requests for the Services they reference. Each only needs to
// enqueue the Services directly referenced — extractor-driven.

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

// refsToRequests dedupes a ref list to one Request per (NS, Name). Used
// by the discovery-resource mappers — multiple webhooks in one config
// can reference the same Service; we only need to reconcile it once.
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
