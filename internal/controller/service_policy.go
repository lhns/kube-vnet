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
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Event reasons on Services, from both Service-owned policy reconcilers.
const (
	// ReasonNamedPortUnresolved (Normal): a named targetPort has no backing
	// pod that declares it yet, expected while pods start. Emitted when the
	// wait starts, not on every retry.
	ReasonNamedPortUnresolved = "NamedPortUnresolved"
	// ReasonServiceHasNoSelector (Warning): an exposed Service has no
	// selector, so no policy can select its pods and they stay blocked.
	ReasonServiceHasNoSelector = "ServiceHasNoSelector"
)

// errNamedPortUnresolvable signals that the Service references a named
// targetPort that no backing pod currently exposes. The reconciler treats
// this as a transient state, surfaces an Event on the Service, and requeues.
var errNamedPortUnresolvable = errors.New("named targetPort unresolvable: no backing pod with matching port name")

// serviceReconciler is what serviceSource.reconcile needs of its reconciler.
type serviceReconciler struct {
	client.Client
	scheme   *runtime.Scheme
	nsFilter *NamespaceFilter
	rec      events.EventRecorder
	restores *policyTracker
	pending  *onceSet
}

// serviceSource is one kind of Service-owned external-allow policy. The
// Service-source (ADR 0038) and apiserver-reachable (ADR 0041) policies share
// owner and role and differ only by source kind.
type serviceSource struct {
	kind   string // LabelSourceKind value
	prefix string // SourceLabelValue prefix
	// errorKind is the apply_errors_total kind. For Events, what names the
	// policy and who the peer it lets in.
	errorKind, what, who string
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
	errorKind:   ApplyErrorExternalAllow,
	what:        "external-allow",
	who:         "external clients",
	sweepLabels: map[string]string{LabelRole: LabelRoleExternalAllow},
	skip:        claimedByOtherSourceKind,
}

var apiserverPolicies = serviceSource{
	kind:        LabelSourceKindApiserver,
	prefix:      "apiserver-",
	errorKind:   ApplyErrorApiserverReachable,
	what:        "apiserver-reachable",
	who:         "the kube-apiserver",
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

// reconcile is the Reconcile of both Service-owned policy reconcilers.
// desired returns the Service's policy, nil for none, or
// errNamedPortUnresolvable while a named targetPort has no backing pod.
func (s serviceSource) reconcile(ctx context.Context, req ctrl.Request, r serviceReconciler,
	desired func(context.Context, *corev1.Service) (*networkingv1.NetworkPolicy, error),
) (ctrl.Result, error) {
	// A reconcile that removes the policy (Service or namespace gone, swept)
	// must not make its next creation look like a restore. One that fails
	// keeps the key: the policy is still wanted, and recreating it after a
	// delete is a restore however many attempts it takes.
	forget := func() {
		r.restores.forget(client.ObjectKey{Namespace: req.Namespace, Name: servicePolicyName(s.kind, req.Namespace, req.Name)})
	}
	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			forget()
			r.pending.forget(req.NamespacedName)
			return ctrl.Result{}, s.deleteByServiceKey(ctx, r.Client, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}
	// Any outcome but another wait ends the current one, so the next is reported.
	waiting := false
	defer func() {
		if !waiting {
			r.pending.clear(svc, ReasonNamedPortUnresolved)
		}
	}()

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			forget()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// NamespaceLifecycle admission rejects creates in a terminating namespace.
	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	if !r.nsFilter.IsManaged(ns) || ExternalAllowOptedOut(ns.Annotations) || ExternalAllowOptedOut(svc.Annotations) {
		forget()
		return ctrl.Result{}, s.sweep(ctx, r.Client, svc, "")
	}

	policy, err := desired(ctx, svc)
	if errors.Is(err, errNamedPortUnresolvable) {
		waiting = true
		if r.pending.first(svc, ReasonNamedPortUnresolved) {
			eventf(r.rec, svc, corev1.EventTypeNormal, ReasonNamedPortUnresolved, "Reconcile",
				"the %s policy waits for a running pod behind this Service that declares its named targetPort; "+
					"until then %s cannot reach that port. Normal while pods start; if it lasts, compare the "+
					"Service's targetPort names with the pods' containerPort names.", s.what, s.who)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if policy == nil {
		forget()
		return ctrl.Result{}, s.sweep(ctx, r.Client, svc, "")
	}

	// The Service as controller owner lets GC delete the policy with it, even
	// while the operator is down.
	if err := controllerutil.SetControllerReference(svc, policy, r.scheme); err != nil {
		return ctrl.Result{}, err
	}
	created, err := applyPolicy(ctx, r.Client, r.Client, policy)
	if err != nil {
		applyFailed(r.rec, svc, s.errorKind,
			"the %s NetworkPolicy %s could not be applied: %v. Until this is fixed, %s cannot reach this Service's pods.",
			s.what, policy.Name, err, s.who)
		return ctrl.Result{}, err
	}
	if r.restores.applied(client.ObjectKeyFromObject(policy), created) {
		policyRestored(r.rec, policy,
			"this NetworkPolicy was deleted and has been recreated: kube-vnet lets %s reach Service %s through it. "+
				"To opt the Service out, annotate it %s=false.", s.who, svc.Name, AnnotationExternalAllow)
	}
	// Sweeps this source's other policies of svc: stale or legacy names.
	return ctrl.Result{}, s.sweep(ctx, r.Client, svc, policy.Name)
}

// sweep deletes svc's policies of this source except the one named keep
// ("" keeps none). Ownership is the controller owner reference, which
// survives label-scheme changes, so legacy policies are cleaned up too.
func (s serviceSource) sweep(ctx context.Context, c client.Client, svc *corev1.Service, keep string) error {
	return sweepStalePolicies(ctx, c,
		inNamespacePolicyLabels(svc.Namespace, s.sweepLabels),
		map[client.ObjectKey]bool{{Namespace: svc.Namespace, Name: keep}: true},
		func(p *networkingv1.NetworkPolicy) bool {
			return !hasControllerOwner(p, "Service", svc.Name, svc.UID) || (s.skip != nil && s.skip(p))
		},
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
		nil, nil,
	)
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

// protocolOrTCP applies the Kubernetes default: an empty protocol means TCP.
func protocolOrTCP(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
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

// servicePolicyName returns `kube-vnet.ext.<sourceKind>.<svcName>-<8hex>`,
// truncating the Service name to stay within the 63-char name limit. The hash
// is over <ns>/<name>, so truncated names stay unique.
func servicePolicyName(sourceKind, ns, name string) string {
	prefix := "kube-vnet." + PolicyKindExternal + "." + sourceKind + "."
	const hashLen = 8
	const maxNameLen = 63
	maxBase := maxNameLen - len(prefix) - 1 - hashLen
	base := name[:min(len(name), maxBase)]
	h := sha256.Sum256([]byte(ns + "/" + name))
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
		Watches(&corev1.Pod{}, podToSelectingServices(c))
}

// namespaceToServices enqueues every Service in a namespace when it changes,
// so a Namespace-level opt-out reaches Services that saw no event.
func namespaceToServices(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return servicesInNamespace(ctx, c, obj.GetName(), nil)
	}
}

// podToSelectingServices enqueues the named-targetPort Services whose
// selector a pod enters or leaves. Only those Services resolve ports from
// pods, and container ports are immutable, so a pod can change a Service's
// resolution only by entering its selector (create, relabel in) or leaving
// it (delete, relabel out). A relabel that flips no selector, such as the
// operator's own kube-vnet.system stamp, enqueues nothing; a Service that
// selects on a stamped label still flips. Matching is labelsMatchSelector,
// the same test resolveTargetPorts applies.
func podToSelectingServices(c client.Reader) handler.Funcs {
	enqueue := func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request], ns string, affected func(selector map[string]string) bool) {
		for _, req := range servicesInNamespace(ctx, c, ns, func(svc *corev1.Service) bool {
			return hasNamedTargetPort(svc) && affected(svc.Spec.Selector)
		}) {
			q.Add(req)
		}
	}
	matches := func(obj client.Object) func(map[string]string) bool {
		return func(sel map[string]string) bool { return labelsMatchSelector(obj.GetLabels(), sel) }
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, q, e.Object.GetNamespace(), matches(e.Object))
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			oldL, newL := e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()
			if maps.Equal(oldL, newL) {
				return
			}
			enqueue(ctx, q, e.ObjectNew.GetNamespace(), func(sel map[string]string) bool {
				return labelsMatchSelector(oldL, sel) != labelsMatchSelector(newL, sel)
			})
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, q, e.Object.GetNamespace(), matches(e.Object))
		},
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
