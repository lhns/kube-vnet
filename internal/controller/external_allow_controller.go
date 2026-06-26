package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
// The emitted policy is additive: it shares the namespace with kube-vnet's
// baseline + membership policies, and NetworkPolicy union semantics mean
// internal pod-to-pod isolation is preserved while the externally-exposed
// targetPort becomes reachable from `ipBlock: 0.0.0.0/0`.
//
// Default-on. Opt-out via the `kube-vnet/external-allow=false` annotation
// on the Service or on its Namespace.
//
// Watches:
//   - corev1.Service                  primary trigger; ports/selector/type/annotations
//   - corev1.Namespace                catches mid-flight kube-vnet/disabled or
//                                     kube-vnet/external-allow flips
//   - networkingv1.NetworkPolicy      drift correction (filtered by role label)
//   - corev1.Pod (Create only)        unblocks named-targetPort resolution when a
//                                     backing pod with the matching container-port
//                                     name appears (closes the up-to-30s requeue
//                                     latency for Service-before-Pod ordering)
//
// Note: hostPort container detection and hostNetwork pod warnings are
// deliberately scoped out of v1; NetworkPolicy enforcement on host-network
// pods is CNI-dependent (see ADR 0038 "out of scope") and per-pod hostPort
// policies need a label-stamping design that's worth its own iteration.
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
			// Service gone. The owner-reference cascade on the apiserver
			// side handles the policy delete; we don't need to do anything.
			return ctrl.Result{}, nil
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

	// Resolve named targetPorts by listing pods in the NS. Pods are watched
	// elsewhere so the cache is already warm; List() is cheap.
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
		// Service isn't externally exposed (ClusterIP without externalIPs,
		// headless, ExternalName, no selector). Clear any stale policy and exit.
		return ctrl.Result{}, r.deletePolicyForService(ctx, svc)
	}

	// Owner-reference so apiserver GC handles cascade-delete when the
	// Service is deleted, even if the operator is briefly down.
	if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	desired.SetResourceVersion("")
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		logger.Error(err, "apply external-allow policy failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// deletePolicyForService removes the operator-emitted external-allow policy
// keyed off this Service, if any. Idempotent.
func (r *ExternalAllowReconciler) deletePolicyForService(ctx context.Context, svc *corev1.Service) error {
	name := externalAllowPolicyName(svc)
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace},
	}
	if err := r.Delete(ctx, pol); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// buildExternalAllowPolicy constructs the desired NetworkPolicy for a single
// externally-exposed Service. Returns:
//
//	(policy, nil)                       — emit this policy
//	(nil, nil)                          — Service isn't externally exposed; no policy
//	(nil, errNamedPortUnresolvable)     — named targetPort cannot be resolved yet
//
// Multi-port Services that contain ONE unresolvable named targetPort return
// the error rather than partial emission — a partial policy creates a
// confusing "this port works, that one doesn't" state. Full requeue.
func buildExternalAllowPolicy(svc *corev1.Service, podsInNS []corev1.Pod) (*networkingv1.NetworkPolicy, error) {
	if !isExternallyExposed(svc) {
		return nil, nil
	}
	if len(svc.Spec.Selector) == 0 {
		// Headless / manually-managed Endpoints. No podSelector to mirror.
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
				LabelManagedBy: LabelManagedByValue,
				LabelRole:      LabelRoleExternalAllow,
				// Bare service name — label values can't contain `/`. The
				// kind ("service") is implied; if we later add Pod-source
				// (hostPort), a separate LabelSourceKind would carry it.
				LabelSource: svc.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: cloneStringMap(svc.Spec.Selector),
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
		// "" is the legacy default (treated as ClusterIP by the apiserver).
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
// in `selector`. Empty selector returns false — Services without a selector
// aren't externally-exposed in the sense we care about (no podSelector to
// mirror; we never reach this function for them).
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

// externalAllowPolicyName returns a deterministic policy name keyed on the
// Service. Format: `kube-vnet.external-<service-name>-<8hex>`, total ≤63
// chars (Kubernetes name limit). The hash makes truncated names unique by
// the original `<namespace>/<name>`.
func externalAllowPolicyName(svc *corev1.Service) string {
	const prefix = "kube-vnet.external-"
	const hashLen = 8
	const maxNameLen = 63
	maxBase := maxNameLen - len(prefix) - 1 - hashLen
	base := svc.Name
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	// Hash the full NS+name so a truncated name doesn't collide with another
	// Service that shares the same truncated prefix.
	h := sha256.Sum256([]byte(svc.Namespace + "/" + svc.Name))
	return prefix + base + "-" + hex.EncodeToString(h[:])[:hashLen]
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *ExternalAllowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Re-enqueue the source Service when an external-allow policy event
	// fires (delete = drift correction; user-edit-then-update = SSA reapply).
	extPolPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[LabelManagedBy] == LabelManagedByValue && l[LabelRole] == LabelRoleExternalAllow
	})

	// Pod creates only — a new pod is the only event that can unblock a
	// previously-unresolvable named targetPort. Pod updates/deletes never
	// help: updates can't add a container port name without recreating the
	// pod, and deletes remove a (possibly-resolving) endpoint without
	// adding new ones. Without this watcher, the named-port pending case
	// recovers only on the 30s requeue.
	podCreateOnly := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("external-allow").
		For(&corev1.Service{}).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(externalAllowPolicyToService),
			builder.WithPredicates(extPolPredicate),
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
		Complete(r)
}

// externalAllowPolicyToService derives the source Service from the
// `kube-vnet.system/source: <name>` label so a deleted/edited policy
// re-enqueues only the affected Service, not the whole NS.
func externalAllowPolicyToService(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[LabelSource]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// namespaceToServices enqueues every Service in a namespace when the NS
// changes. Catches the "annotated kube-vnet/external-allow=false mid-flight"
// case where no Service event fires but the existing policies should go
// away. List is bounded to one NS — cheap.
func (r *ExternalAllowReconciler) namespaceToServices(ctx context.Context, obj client.Object) []reconcile.Request {
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

// podToServicesWithNamedPorts enqueues every Service in the new pod's
// namespace that has at least one named (string-typed) targetPort. Those
// are the only Services whose policy emission might have been blocked
// waiting for this pod's container-port names; numeric-targetPort Services
// don't depend on Pod state at all. Bounded by NS size.
func (r *ExternalAllowReconciler) podToServicesWithNamedPorts(ctx context.Context, obj client.Object) []reconcile.Request {
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

