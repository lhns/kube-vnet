package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
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
}

// errNamedPortUnresolvable signals that the Service references a named
// targetPort that no backing pod currently exposes. The reconciler treats
// this as a transient state, surfaces an Event on the Service, and requeues.
var errNamedPortUnresolvable = errors.New("named targetPort unresolvable: no backing pod with matching port name")

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *ExternalAllowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("service", req.NamespacedName)

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner-ref GC deletes the policy in a real cluster; delete by
			// label too, for when GC doesn't run (envtest).
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

	// Three opt-out gates: NS in --disabled-namespaces or annotated
	// kube-vnet/disabled=true; NS annotated kube-vnet/external-allow=false;
	// or this specific Service annotated kube-vnet/external-allow=false.
	if !r.NSFilter.IsManaged(ns) ||
		ExternalAllowOptedOut(ns.Annotations) ||
		ExternalAllowOptedOut(svc.Annotations) {
		return ctrl.Result{}, r.deletePolicyForService(ctx, svc)
	}

	// Pods resolve named targetPorts.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(svc.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	desired, err := buildExternalAllowPolicy(svc, pods.Items)
	if err != nil {
		if errors.Is(err, errNamedPortUnresolvable) {
			r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, "Pending", "Reconcile",
				"external-allow policy pending: a named targetPort has no backing pod yet")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if desired == nil {
		// Not externally exposed, or no selector to mirror. An exposed
		// Service without a selector (manually-managed Endpoints) gets an
		// Event, since its missing policy is otherwise unexplained.
		if isExternallyExposed(svc) && len(svc.Spec.Selector) == 0 {
			r.Recorder.Eventf(svc, nil, corev1.EventTypeNormal, "Skipped", "Reconcile",
				"external-allow skipped: Service has no spec.selector (manually-managed Endpoints); cannot derive a podSelector. Add a selector or write your own NetworkPolicy.")
		}
		return ctrl.Result{}, r.deletePolicyForService(ctx, svc)
	}

	// Owner ref, so GC deletes the policy with the Service even while the
	// operator is down.
	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply external-allow policy failed")
		return ctrl.Result{}, err
	}

	// Sweep this Service's stale policies by owner ref, which also catches
	// legacy names and labels. claimedByOtherSourceKind spares the
	// apiserver-reachable policy on the same Service.
	keep := map[client.ObjectKey]bool{
		{Namespace: svc.Namespace, Name: desired.Name}: true,
	}
	if err := sweepStalePoliciesByOwner(ctx, r.Client,
		inNamespacePolicyLabels(svc.Namespace, map[string]string{
			LabelRole: LabelRoleExternalAllow,
		}),
		"Service", svc.Name, svc.UID,
		keep,
		claimedByOtherSourceKind,
	); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// claimedByOtherSourceKind reports whether an external-allow policy belongs
// to another reconciler. The apiserver-reachable policy has the same owner
// and role as ours and differs only by source-kind; sweeping it would start a
// delete/recreate loop with that reconciler, leaving windows where the
// apiserver cannot reach the webhook. Policies with no source-kind label
// predate ADR 0039/0040 and are ours to sweep.
func claimedByOtherSourceKind(p *networkingv1.NetworkPolicy) bool {
	sk, ok := p.Labels[LabelSourceKind]
	return ok && sk != LabelSourceKindService
}

// deletePolicyForService removes every Service-source external-allow policy
// owned by this Service, current and legacy names alike.
func (r *ExternalAllowReconciler) deletePolicyForService(ctx context.Context, svc *corev1.Service) error {
	return sweepStalePoliciesByOwner(ctx, r.Client,
		inNamespacePolicyLabels(svc.Namespace, map[string]string{
			LabelRole: LabelRoleExternalAllow,
		}),
		"Service", svc.Name, svc.UID,
		nil, // nothing to keep — sweep them all
		claimedByOtherSourceKind,
	)
}

// deletePolicyByServiceKey handles the Service-NotFound path: with no UID to
// match owner refs against, it deletes by LabelSource. Legacy-format policies
// are left to owner-ref GC.
func (r *ExternalAllowReconciler) deletePolicyByServiceKey(ctx context.Context, namespace, serviceName string) error {
	return sweepStalePolicies(ctx, r.Client,
		inNamespacePolicyLabels(namespace, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: LabelSourceKindService,
			LabelSource:     SourceLabelValue("svc-", namespace, serviceName),
		}),
		nil,
	)
}

// buildExternalAllowPolicy constructs the desired NetworkPolicy for a single
// externally-exposed Service. Returns:
//
//	(policy, nil)                       — emit this policy
//	(nil, nil)                          — Service isn't externally exposed; no policy
//	(nil, errNamedPortUnresolvable)     — named targetPort cannot be resolved yet
//
// One unresolvable named targetPort fails the whole Service rather than
// emitting a partial policy where some ports work and others don't.
func buildExternalAllowPolicy(svc *corev1.Service, podsInNS []corev1.Pod) (*networkingv1.NetworkPolicy, error) {
	if !isExternallyExposed(svc) {
		return nil, nil
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, nil
	}

	ports := make([]networkingv1.NetworkPolicyPort, 0, len(svc.Spec.Ports))
	for _, sp := range svc.Spec.Ports {
		targetPort, err := resolveTargetPort(sp, svc.Spec.Selector, podsInNS)
		if err != nil {
			return nil, err
		}
		proto := sp.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		portVal := intstr.FromInt32(targetPort)
		ports = append(ports, networkingv1.NetworkPolicyPort{
			Protocol: &proto,
			Port:     &portVal,
		})
	}

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalAllowPolicyName(svc),
			Namespace: svc.Namespace,
			Labels: map[string]string{
				LabelManagedBy:    LabelManagedByValue,
				LabelK8sManagedBy: LabelManagedByValue,
				LabelRole:         LabelRoleExternalAllow,
				LabelSourceKind:   LabelSourceKindService,
				LabelSource:       SourceLabelValue("svc-", svc.Namespace, svc.Name),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: maps.Clone(svc.Spec.Selector),
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
					},
					Ports: ports,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}, nil
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

// resolveTargetPort returns the numeric pod-side port for a Service port.
//
// Three cases:
//
//	type=Int, IntVal=0     — TargetPort unset; defaults to Service Port
//	                         (standard K8s convention).
//	type=Int               — Use IntVal directly.
//	type=String            — Named targetPort; look up the container port
//	                         named StrVal on any backing pod (matching the
//	                         Service's selector). Returns errNamedPortUnresolvable
//	                         if no matching pod or no matching named port.
func resolveTargetPort(sp corev1.ServicePort, selector map[string]string, pods []corev1.Pod) (int32, error) {
	switch sp.TargetPort.Type {
	case intstr.Int:
		if sp.TargetPort.IntVal == 0 {
			return sp.Port, nil
		}
		return sp.TargetPort.IntVal, nil
	case intstr.String:
		name := sp.TargetPort.StrVal
		if name == "" {
			return sp.Port, nil
		}
		for _, p := range pods {
			if !labelsMatchSelector(p.Labels, selector) {
				continue
			}
			for _, c := range p.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == name {
						return cp.ContainerPort, nil
					}
				}
			}
		}
		return 0, errNamedPortUnresolvable
	}
	return sp.Port, nil
}

// labelsMatchSelector returns true if `labels` contains every key/value pair
// in `selector`. An empty selector matches nothing.
func labelsMatchSelector(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// externalAllowPolicyName returns the Service-source policy name,
// `kube-vnet.ext.svc.<svcName>-<8hex>` (ADR 0039).
func externalAllowPolicyName(svc *corev1.Service) string {
	return servicePolicyName(PolicySourceKindService, svc)
}

// servicePolicyName returns `kube-vnet.ext.<sourceKind>.<svcName>-<8hex>`,
// truncating the Service name to stay within the 63-char name limit. The hash
// is over <ns>/<name>, so truncated names stay unique.
func servicePolicyName(sourceKind string, svc *corev1.Service) string {
	prefix := "kube-vnet." + PolicyKindExternal + "." + sourceKind + "."
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

// externalAllowPolicyPredicate bounds a NetworkPolicy watch to managed
// external-allow policies of one source kind. The Service-source and
// apiserver-reachable policies share an owner (the Service) and a role, and
// both watches enqueue by owner reference, so the source-kind check is what
// keeps those two reconcilers off each other's policies.
func externalAllowPolicyPredicate(sourceKind string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue &&
			l[LabelRole] == LabelRoleExternalAllow &&
			l[LabelSourceKind] == sourceKind
	})
}

// podCreateOnly passes pod creates only: a new pod is the only event that can
// unblock a previously-unresolvable named targetPort. Updates can't add a
// container port name without recreating the pod, and deletes only remove
// candidates. Without this watch the pending case recovers only on the 30s
// requeue.
var podCreateOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

func (r *ExternalAllowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("external-allow").
		For(&corev1.Service{}).
		Watches(
			&networkingv1.NetworkPolicy{},
			// By owner ref: LabelSource is length-bounded and doesn't
			// round-trip to a Service name (ADR 0011).
			handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(),
				&corev1.Service{}, handler.OnlyControllerOwner()),
			builder.WithPredicates(externalAllowPolicyPredicate(LabelSourceKindService)),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(namespaceToServices(r.Client)),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(podToServicesWithNamedPorts(r.Client)),
			builder.WithPredicates(podCreateOnly),
		).
		Complete(r)
}

// namespaceToServices enqueues every Service in a namespace when it changes,
// so a Namespace-level opt-out reaches Services that saw no event.
func namespaceToServices(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return servicesInNamespace(ctx, c, obj.GetName(), nil)
	}
}

// podToServicesWithNamedPorts enqueues the Services in the pod's namespace
// that use a named targetPort, the only ones whose emission can be waiting on
// a pod.
func podToServicesWithNamedPorts(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return servicesInNamespace(ctx, c, obj.GetNamespace(), hasNamedTargetPort)
	}
}

// servicesInNamespace returns a request per Service in ns that passes keep
// (every Service if keep is nil).
func servicesInNamespace(ctx context.Context, c client.Reader, ns string, keep func(*corev1.Service) bool) []reconcile.Request {
	var svcs corev1.ServiceList
	if err := c.List(ctx, &svcs, client.InNamespace(ns)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range svcs.Items {
		if keep != nil && !keep(&svcs.Items[i]) {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&svcs.Items[i]),
		})
	}
	return out
}

// hasNamedTargetPort returns true if any Service port uses a string
// (named) targetPort that needs Pod-side resolution.
func hasNamedTargetPort(svc *corev1.Service) bool {
	for _, p := range svc.Spec.Ports {
		if p.TargetPort.Type == intstr.String && p.TargetPort.StrVal != "" {
			return true
		}
	}
	return false
}
