package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
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
	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, svcSourcePolicies.deleteByServiceKey(ctx, r.Client, req.Namespace, req.Name)
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

	if !r.NSFilter.IsManaged(ns) ||
		ExternalAllowOptedOut(ns.Annotations) ||
		ExternalAllowOptedOut(svc.Annotations) {
		return ctrl.Result{}, svcSourcePolicies.sweep(ctx, r.Client, svc, "")
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
		return ctrl.Result{}, svcSourcePolicies.sweep(ctx, r.Client, svc, "")
	}
	return ctrl.Result{}, svcSourcePolicies.apply(ctx, r.Client, r.Scheme, svc, desired)
}

// serviceSource is one kind of Service-owned external-allow policy. The
// Service-source (ADR 0038) and apiserver-reachable (ADR 0041) policies share
// owner and role and differ only by source kind.
type serviceSource struct {
	kind   string // LabelSourceKind value
	prefix string // SourceLabelValue prefix
	// sweepLabels narrows the owner-ref sweep's List; skip, if set, spares
	// policies that List cannot exclude.
	sweepLabels map[string]string
	skip        func(*networkingv1.NetworkPolicy) bool
}

// svcSourcePolicies sweeps by role only, to also catch policies that predate
// the source-kind label (ADR 0039/0040); claimedByOtherSourceKind spares the
// apiserver-reachable policy on the same Service.
var svcSourcePolicies = serviceSource{
	kind:        LabelSourceKindService,
	prefix:      "svc-",
	sweepLabels: map[string]string{LabelRole: LabelRoleExternalAllow},
	skip:        claimedByOtherSourceKind,
}

var apiserverPolicies = serviceSource{
	kind:        LabelSourceKindApiserver,
	prefix:      "apiserver-",
	sweepLabels: map[string]string{LabelRole: LabelRoleExternalAllow, LabelSourceKind: LabelSourceKindApiserver},
}

// claimedByOtherSourceKind reports whether an external-allow policy belongs
// to another reconciler. Sweeping the apiserver-reachable policy would start
// a delete/recreate loop with its reconciler, leaving windows where the
// apiserver cannot reach the webhook. Policies with no source-kind label
// predate ADR 0039/0040 and are ours to sweep.
func claimedByOtherSourceKind(p *networkingv1.NetworkPolicy) bool {
	sk, ok := p.Labels[LabelSourceKind]
	return ok && sk != LabelSourceKindService
}

// policy returns this source's policy for svc, letting cidr reach ports on
// the Service's pods.
func (s serviceSource) policy(svc *corev1.Service, name, cidr string, ports []networkingv1.NetworkPolicyPort) *networkingv1.NetworkPolicy {
	return externalAllowPolicy(name, svc.Namespace, s.kind, SourceLabelValue(s.prefix, svc.Namespace, svc.Name),
		maps.Clone(svc.Spec.Selector), cidr, ports)
}

// apply applies desired with svc as controller owner, so GC deletes it with
// the Service even while the operator is down, then sweeps svc's other
// policies of this source (stale or legacy names).
func (s serviceSource) apply(ctx context.Context, c client.Client, scheme *runtime.Scheme, svc *corev1.Service, desired *networkingv1.NetworkPolicy) error {
	if err := controllerutil.SetControllerReference(svc, desired, scheme); err != nil {
		return err
	}
	if err := c.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return err
	}
	return s.sweep(ctx, c, svc, desired.Name)
}

// sweep deletes svc's policies of this source except the one named keep
// ("" keeps none).
func (s serviceSource) sweep(ctx context.Context, c client.Client, svc *corev1.Service, keep string) error {
	return sweepStalePoliciesByOwner(ctx, c,
		inNamespacePolicyLabels(svc.Namespace, s.sweepLabels),
		"Service", svc.Name, svc.UID,
		map[client.ObjectKey]bool{{Namespace: svc.Namespace, Name: keep}: true},
		s.skip,
	)
}

// deleteByServiceKey deletes by LabelSource for a Service that is gone, whose
// UID owner refs can no longer be matched against. Owner-ref GC removes the
// policies in a real cluster; this covers envtest, which has no GC. Policies
// without the label are left to GC.
func (s serviceSource) deleteByServiceKey(ctx context.Context, c client.Client, ns, name string) error {
	return sweepStalePolicies(ctx, c,
		inNamespacePolicyLabels(ns, map[string]string{
			LabelRole:       LabelRoleExternalAllow,
			LabelSourceKind: s.kind,
			LabelSource:     SourceLabelValue(s.prefix, ns, name),
		}),
		nil,
	)
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

// externalAllowPolicy returns an ingress policy letting cidr reach ports on
// the pods matching podSelector.
func externalAllowPolicy(name, ns, sourceKind, source string, podSelector map[string]string, cidr string, ports []networkingv1.NetworkPolicyPort) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy:    LabelManagedByValue,
				LabelK8sManagedBy: LabelManagedByValue,
				LabelRole:         LabelRoleExternalAllow,
				LabelSourceKind:   sourceKind,
				LabelSource:       source,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}},
				Ports: ports,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
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

// resolveTargetPorts returns the sorted pod-side ports a Service port reaches.
// An unset targetPort defaults to the Service port. A named targetPort
// resolves per backing pod, as the endpoints controller does, so pods that
// map the name to different numbers (mid-rollout) each get theirs; with no
// backing pod declaring the name it returns errNamedPortUnresolvable.
func resolveTargetPorts(sp corev1.ServicePort, selector map[string]string, pods []corev1.Pod) ([]int32, error) {
	tp := sp.TargetPort
	if tp.Type == intstr.Int && tp.IntVal != 0 {
		return []int32{tp.IntVal}, nil
	}
	if tp.Type != intstr.String || tp.StrVal == "" {
		return []int32{sp.Port}, nil
	}
	var out []int32
	for _, p := range pods {
		if !labelsMatchSelector(p.Labels, selector) {
			continue
		}
		for c := range namedPortContainers(&p.Spec) {
			for _, cp := range c.Ports {
				if cp.Name == tp.StrVal {
					out = append(out, cp.ContainerPort)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, errNamedPortUnresolvable
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// appendPolicyPort appends (proto, port) to ports unless already present.
func appendPolicyPort(ports []networkingv1.NetworkPolicyPort, proto corev1.Protocol, port int32) []networkingv1.NetworkPolicyPort {
	for _, p := range ports {
		if *p.Protocol == proto && p.Port.IntVal == port {
			return ports
		}
	}
	portVal := intstr.FromInt32(port)
	return append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &portVal})
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
	return servicePolicyName(LabelSourceKindService, svc)
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

// backingPodChanged passes the pod events that can change how a named
// targetPort resolves: creates, deletes, and label changes (which move a pod
// into or out of a Service's selector). Container ports are immutable, so no
// other update matters.
var backingPodChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		return !maps.Equal(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels())
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

func (r *ExternalAllowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("external-allow").
		For(&corev1.Service{})
	return svcSourcePolicies.watchInputs(b, mgr, r.Client).Complete(r)
}

// watchInputs adds the watches both Service-owned policy reconcilers need
// besides the Service itself: their policies, Namespaces (for a
// Namespace-level opt-out) and pods (for named targetPorts).
func (s serviceSource) watchInputs(b *builder.Builder, mgr ctrl.Manager, c client.Reader) *builder.Builder {
	return b.
		Watches(
			&networkingv1.NetworkPolicy{},
			// By owner ref: LabelSource is length-bounded and doesn't
			// round-trip to a Service name (ADR 0011).
			handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(),
				&corev1.Service{}, handler.OnlyControllerOwner()),
			builder.WithPredicates(externalAllowPolicyPredicate(s.kind)),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(namespaceToServices(c)),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(podToServicesWithNamedPorts(c)),
			builder.WithPredicates(backingPodChanged),
		)
}

// namespaceToServices enqueues every Service in a namespace when it changes,
// so a Namespace-level opt-out reaches Services that saw no event.
func namespaceToServices(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return servicesInNamespace(ctx, c, obj.GetName(), nil)
	}
}

// podToServicesWithNamedPorts enqueues the Services in the pod's namespace
// that use a named targetPort, the only ones whose policy depends on pods.
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
