package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Condition reasons surfaced on VirtualNetwork.status.conditions. Some also
// name Events: InvalidDirection is the pod Event for a join label whose value
// is not a direction (where the direction-value VAP is absent), and
// NamespaceExcluded the pod Event for a join label in an unmanaged namespace.
const (
	ReasonPoliciesGenerated     = "PoliciesGenerated"
	ReasonNoMembers             = "NoMembers"
	ReasonInvalidJoiners        = "InvalidJoiners"
	ReasonHomeNamespaceExcluded = "HomeNamespaceExcluded"
	ReasonApplyFailed           = "ApplyFailed"
	ReasonInvalidName           = "InvalidName"
	ReasonNamespaceNotAllowed   = "NamespaceNotAllowed"
	ReasonNamespaceExcluded     = "NamespaceExcluded"
	ReasonInvalidDirection      = "InvalidDirection"
	ReasonNoIssues              = "NoIssues"
)

// Event reasons (Kubernetes Event.Reason — short, stable, machine-readable).
const (
	EventReady          = "Ready"
	EventNotReady       = "NotReady"
	EventDegraded       = "Degraded"
	EventRecovered      = "Recovered"
	EventApplyFailed    = "ApplyFailed"
	EventPolicyRestored = "PolicyRestored"
)

// nameRegex enforces DNS-1123 label format on VirtualNetwork names (no dots).
// The CRD also enforces this via x-kubernetes-validations; this is defense in depth.
var nameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// VirtualNetworkReconciler reconciles VirtualNetwork resources into NetworkPolicies.
type VirtualNetworkReconciler struct {
	client.Client
	// APIReader is an uncached reader, used to tell reliably whether a policy
	// was absent before we applied it (PolicyRestored). Nil falls back to
	// the cached client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	NSFilter  *NamespaceFilter
	// OperatorNamespace is where the `cluster` system vnet lives. Pod events
	// that name bare `cluster` are routed there.
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// Events need both groups. Every reconciler emits through
// mgr.GetEventRecorder, controller-runtime's events.k8s.io/v1 recorder, so
// without the second rule every Event the operator writes is forbidden and the
// logs fill with RBAC errors. The core "" rule stays because controller-runtime's
// leader election still uses the deprecated core-v1 recorder. envtest does not
// enforce RBAC, so only the manifest test in rbac_aggregated_integration_test.go
// guards this.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile implements the controller-runtime Reconciler interface.
func (r *VirtualNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx).WithValues("vnet", req.NamespacedName)
	start := time.Now()
	defer func() { observeReconcile(start, err) }()

	vnet := &vnetv1alpha1.VirtualNetwork{}
	if err := r.Get(ctx, req.NamespacedName, vnet); err != nil {
		if apierrors.IsNotFound(err) {
			clearMembers(req.Namespace, req.Name)
			return ctrl.Result{}, r.deleteMembershipPolicies(ctx, req.Namespace, req.Name, nil, nil)
		}
		return ctrl.Result{}, err
	}
	if !vnet.DeletionTimestamp.IsZero() {
		clearMembers(vnet.Namespace, vnet.Name)
		return ctrl.Result{}, r.deleteMembershipPolicies(ctx, vnet.Namespace, vnet.Name, nil, nil)
	}

	// Snapshot the stored status for transition events and for updateStatus's
	// no-op check. Must happen before any setReady/setDegraded, which mutate
	// vnet.Status in place.
	priorReady := conditionStatus(vnet, "Ready")
	priorDegraded := conditionStatus(vnet, "Degraded")
	storedStatus := vnet.Status.DeepCopy()

	// Defense-in-depth name validation. The CRD CEL rule should already reject names with dots.
	if !nameRegex.MatchString(vnet.Name) {
		setReady(vnet, metav1.ConditionFalse, ReasonInvalidName,
			fmt.Sprintf("name %q is not a DNS-1123 label", vnet.Name))
		setDegraded(vnet, metav1.ConditionTrue, ReasonInvalidName,
			fmt.Sprintf("VirtualNetwork name %q must match %s", vnet.Name, nameRegex.String()))
		_ = r.updateStatus(ctx, vnet, nil, nil, storedStatus)
		r.emitTransitionEvents(vnet, priorReady, priorDegraded)
		return ctrl.Result{}, nil
	}

	// Reject an unmanaged home namespace. System vnets are exempt: the
	// cluster vnet's home is the operator namespace, which cmd/main.go always
	// disables as a privilege boundary. The system-vnet VAP keeps the
	// managed-by label honest.
	isSystem := vnet.Labels[LabelManagedBy] == LabelManagedByValue
	homeManaged, err := r.NSFilter.Manages(ctx, r.Client, vnet.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isSystem && !homeManaged {
		setReady(vnet, metav1.ConditionFalse, ReasonHomeNamespaceExcluded,
			fmt.Sprintf("home namespace %q is excluded by the operator", vnet.Namespace))
		setDegraded(vnet, metav1.ConditionTrue, ReasonHomeNamespaceExcluded,
			fmt.Sprintf("home namespace %q is in the operator excluded list or has kube-vnet/disabled=true", vnet.Namespace))
		_ = r.updateStatus(ctx, vnet, nil, nil, storedStatus)
		r.emitTransitionEvents(vnet, priorReady, priorDegraded)
		setMembers(vnet.Namespace, vnet.Name, 0)
		// Remove policies from earlier reconciles; a failure must retry, or
		// the stale grants stay.
		return ctrl.Result{}, r.deleteMembershipPolicies(ctx, vnet.Namespace, vnet.Name, nil, nil)
	}

	members, invalid, err := r.discoverMembers(ctx, vnet)
	if err != nil {
		return ctrl.Result{}, err
	}

	out := Generate(GenerateInput{
		VNet:        vnet,
		MembersByNS: members,
	})

	desiredKeys := make(map[client.ObjectKey]bool, len(out.Policies))
	policyRefs := make([]vnetv1alpha1.PolicyRef, 0, len(out.Policies))
	// A failed apply doesn't stop the loop: one namespace's quota, webhook or
	// RBAC failure must not leave every later namespace without its policy
	// until the retry. The errors are joined and returned after the rest.
	var applyErrs []error
	failedNS := map[string]bool{}
	// A policy created now that the last status listed was deleted by
	// someone; one not listed is simply new.
	listed := make(map[client.ObjectKey]bool, len(storedStatus.GeneratedPolicies))
	for _, ref := range storedStatus.GeneratedPolicies {
		listed[client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}] = true
	}
	// The uncached reader, so a stale cache can't hide a real deletion.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	for i := range out.Policies {
		p := &out.Policies[i]
		desiredKeys[client.ObjectKeyFromObject(p)] = true
		// Don't apply into a terminating namespace: NamespaceLifecycle
		// admission rejects the create once the namespace controller has
		// deleted the policy. Its pods still count as members until they are
		// gone. The key stays desired so the sweep leaves the namespace alone.
		terminating, err := r.namespaceTerminating(ctx, p.Namespace)
		if err == nil && terminating {
			continue
		}
		created := false
		if err == nil {
			created, err = applyPolicy(ctx, r.Client, reader, p)
		}
		if err != nil {
			logger.Error(err, "apply policy failed", "policy", p.Namespace+"/"+p.Name)
			// On the desired policy, so the Event lands in the member
			// namespace even when the policy was never created. It names
			// only this namespace's own failure.
			applyFailed(r.Recorder, p, ApplyErrorMembershipPolicy,
				"NetworkPolicy %s for VirtualNetwork %s/%s could not be applied in this namespace: %v. "+
					"Until this is fixed, member pods here receive no traffic from the vnet's other members.",
				p.Name, vnet.Namespace, vnet.Name, err)
			applyErrs = append(applyErrs, fmt.Errorf("namespace %s: %w", p.Namespace, err))
			failedNS[p.Namespace] = true
			continue
		}
		if created && listed[client.ObjectKeyFromObject(p)] {
			policyRestored(r.Recorder, vnet, "recreated previously-deleted policy %s/%s", p.Namespace, p.Name)
			// Also on the policy, in the namespace of whoever deleted it.
			policyRestored(r.Recorder, p,
				"this NetworkPolicy was deleted and has been recreated: kube-vnet manages it for VirtualNetwork %s/%s. "+
					"To take pods out of the vnet, remove their join labels, bindings or baseline entries instead.",
				vnet.Namespace, vnet.Name)
		}
		policyRefs = append(policyRefs, vnetv1alpha1.PolicyRef{Namespace: p.Namespace, Name: p.Name})
	}

	// The sweep runs even after a failed apply: a stale policy grants access
	// the vnet no longer allows. Desired keys are never swept, applied or not,
	// and namespaces with a failed apply are spared entirely, so a policy
	// under an older name stays until its replacement exists.
	sweepErr := r.deleteMembershipPolicies(ctx, vnet.Namespace, vnet.Name, desiredKeys, failedNS)
	if sweepErr != nil && len(applyErrs) == 0 {
		return ctrl.Result{}, sweepErr
	}

	if len(invalid) > 0 {
		parts := make([]string, len(invalid))
		for i, j := range invalid {
			parts[i] = fmt.Sprintf("%s/%s:%s", j.PodNamespace, j.PodName, j.Reason)
		}
		setDegraded(vnet, metav1.ConditionTrue, ReasonInvalidJoiners,
			fmt.Sprintf("%s: %s",
				pluralize(len(invalid), "1 invalid joiner", "%d invalid joiners"),
				firstFew(parts, ", ", "(+%d more)")))
	} else {
		setDegraded(vnet, metav1.ConditionFalse, ReasonNoIssues, "")
	}
	switch {
	case len(applyErrs) > 0:
		msg := fmt.Sprintf("%d of %s failed to apply: %s", len(applyErrs),
			pluralize(len(out.Policies), "1 NetworkPolicy", "%d NetworkPolicies"),
			joinErrorMessages(applyErrs))
		setReady(vnet, metav1.ConditionFalse, ReasonApplyFailed, msg)
		// One summary per reconcile; each failed namespace has its own
		// Event on its policy.
		eventf(r.Recorder, vnet, corev1.EventTypeWarning, EventApplyFailed, "Apply", "%s", msg)
	case len(out.Policies) == 0:
		setReady(vnet, metav1.ConditionTrue, ReasonNoMembers, "no pods are joining this VirtualNetwork")
	default:
		setReady(vnet, metav1.ConditionTrue, ReasonPoliciesGenerated,
			fmt.Sprintf("%s in %s",
				pluralize(len(out.Policies), "1 NetworkPolicy", "%d NetworkPolicies"),
				pluralize(len(members), "1 namespace", "%d namespaces")))
	}

	statusErr := r.updateStatus(ctx, vnet, members, policyRefs, storedStatus)
	if statusErr != nil && len(applyErrs) == 0 {
		return ctrl.Result{}, statusErr
	}
	r.emitTransitionEvents(vnet, priorReady, priorDegraded)

	totalMembers := 0
	for _, byDir := range members {
		totalMembers += len(uniquePods(byDir))
	}
	setMembers(vnet.Namespace, vnet.Name, totalMembers)

	if len(applyErrs) > 0 {
		return ctrl.Result{}, errors.Join(append(applyErrs, sweepErr, statusErr)...)
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// joinErrorMessages joins errs into one line for a condition message: a
// message over the CRD's 32768-byte limit would fail the status write, and
// many namespaces tend to fail alike.
func joinErrorMessages(errs []error) string {
	msgs := make([]string, len(errs))
	for i, err := range errs {
		msgs[i] = err.Error()
	}
	return firstFew(msgs, "; ", "and %d more")
}

// firstFew joins the first three items with sep and counts the rest with the
// format more.
func firstFew(items []string, sep, more string) string {
	const n = 3
	if len(items) > n {
		items = append(items[:n:n], fmt.Sprintf(more, len(items)-n))
	}
	return strings.Join(items, sep)
}

// namespaceTerminating reports whether namespace name is being deleted.
func (r *VirtualNetworkReconciler) namespaceTerminating(ctx context.Context, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return ns.DeletionTimestamp != nil, nil
}

// discoverMembers lists pods cluster-wide and partitions them into the
// generator's MembersByNS shape (namespace → direction → pods). Membership is
// the canonical system label (SystemLabelKey) stamped by resolution; user
// join labels are only scanned for InvalidJoiner diagnostics.
func (r *VirtualNetworkReconciler) discoverMembers(
	ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork,
) (members map[string]map[Direction][]string, invalid []InvalidJoiner, err error) {
	members = map[string]map[Direction][]string{}
	sysKey := SystemLabelKey(vnet.Namespace, vnet.Name)
	userBareKey := userJoinPrefix + vnet.Name
	userPrefixedKey := userJoinPrefix + vnet.Namespace + "." + vnet.Name
	clusterVnet := vnet.Name == SystemVnetCluster

	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil, nil, err
	}

	// Memoized per namespace: namespaces repeat heavily across the pod list.
	nsAdmitted := map[string]bool{}
	admits := func(ns string) (bool, error) {
		if ok, seen := nsAdmitted[ns]; seen {
			return ok, nil
		}
		ok, err := PermitsForVnet(ctx, r.Client, vnet, ns)
		nsAdmitted[ns] = ok
		return ok, err
	}
	// ineligible returns why pods in ns cannot be members, or "" if they can.
	nsReason := map[string]string{}
	ineligible := func(ns string) (string, error) {
		if reason, seen := nsReason[ns]; seen {
			return reason, nil
		}
		managed, err := r.NSFilter.Manages(ctx, r.Client, ns)
		if err != nil {
			return "", err
		}
		reason := ""
		if !managed {
			reason = ReasonNamespaceExcluded
		} else if ok, err := admits(ns); err != nil {
			return "", err
		} else if !ok {
			reason = ReasonNamespaceNotAllowed
		}
		nsReason[ns] = reason
		return reason, nil
	}

	for i := range pods.Items {
		p := &pods.Items[i]

		// Diagnostic scan on user join labels. Advisory only: it must not
		// gate membership, since a pod stamped via a binding or baseline
		// stays a member even if it also carries a malformed join label.
		// The bare form names a vnet in the pod's own namespace, except
		// `cluster`, which is reachable by its bare name from anywhere.
		var userVals []string
		if v, ok := p.Labels[userBareKey]; ok && (p.Namespace == vnet.Namespace || clusterVnet) {
			userVals = append(userVals, v)
		}
		if v, ok := p.Labels[userPrefixedKey]; ok && !clusterVnet {
			userVals = append(userVals, v)
		}
		// Only namespaces the vnet admits count. Anyone can label a pod with
		// any vnet's name, so a joiner from elsewhere would let any tenant
		// make this vnet Degraded and write their namespace and pod name
		// into its status. That pod gets its own Event in its namespace.
		if len(userVals) > 0 {
			if ok, err := admits(p.Namespace); err != nil {
				return nil, nil, err
			} else if !ok {
				userVals = nil
			}
		}
		if len(userVals) > 0 {
			reason := ""
			for _, v := range userVals {
				if _, ok := ParseBareDirection(v); !ok {
					reason = ReasonInvalidDirection
				}
			}
			if reason == "" {
				if reason, err = ineligible(p.Namespace); err != nil {
					return nil, nil, err
				}
			}
			if reason != "" {
				invalid = append(invalid, InvalidJoiner{PodNamespace: p.Namespace, PodName: p.Name, Reason: reason})
			}
		}

		if !isStampedMember(p, sysKey) {
			continue
		}

		// Defense in depth against stale stamps. Resolution strips stamps a
		// namespace is no longer granted, but only after its own watch fires.
		// The membership policy must not trust a stamp the current cluster
		// state wouldn't grant.
		if reason, err := ineligible(p.Namespace); err != nil {
			return nil, nil, err
		} else if reason != "" {
			continue
		}

		if members[p.Namespace] == nil {
			members[p.Namespace] = map[Direction][]string{}
		}
		dir := Direction(p.Labels[sysKey])
		members[p.Namespace][dir] = append(members[p.Namespace][dir], p.Name)
	}

	return members, invalid, nil
}

// deleteMembershipPolicies deletes the vnet's membership policies, in every
// namespace, except those in keep (nil deletes all) and those in the
// namespaces of spareNS. The baseline belongs to NamespaceReconciler and is
// never touched here.
func (r *VirtualNetworkReconciler) deleteMembershipPolicies(
	ctx context.Context, homeNS, name string, keep map[client.ObjectKey]bool, spareNS map[string]bool,
) error {
	return sweepStalePolicies(ctx, r.Client, []client.ListOption{client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelNetwork:   homeNS + "." + name,
	}}, keep, func(p *networkingv1.NetworkPolicy) bool { return spareNS[p.Namespace] })
}

// uniquePods flattens a direction → pods map into a sorted, deduplicated list.
func uniquePods(byDir map[Direction][]string) []string {
	var out []string
	for _, pods := range byDir {
		out = append(out, pods...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// updateStatus writes status fields via the subresource, flattening members
// to one pod list per namespace.
func (r *VirtualNetworkReconciler) updateStatus(
	ctx context.Context,
	vnet *vnetv1alpha1.VirtualNetwork,
	members map[string]map[Direction][]string,
	policies []vnetv1alpha1.PolicyRef,
	stored *vnetv1alpha1.VirtualNetworkStatus,
) error {
	out := make([]vnetv1alpha1.NamespaceMembers, 0, len(members))
	for ns, byDir := range members {
		out = append(out, vnetv1alpha1.NamespaceMembers{Namespace: ns, Pods: uniquePods(byDir)})
	}
	slices.SortFunc(out, func(a, b vnetv1alpha1.NamespaceMembers) int { return strings.Compare(a.Namespace, b.Namespace) })

	// Skip the write when nothing changed: the For() watch would otherwise
	// re-enqueue the vnet on every write, a self-feeding loop. SetStatusCondition
	// keeps LastTransitionTime unless the status flips, so the comparison holds.
	// `stored` is the status as fetched, before setReady/setDegraded mutated it.
	vnet.Status.Members = out
	vnet.Status.GeneratedPolicies = policies
	vnet.Status.ObservedGeneration = vnet.Generation
	if stored != nil && equality.Semantic.DeepEqual(stored, &vnet.Status) {
		return nil
	}
	return r.Status().Update(ctx, vnet)
}

// emitTransitionEvents emits Kubernetes Events when Ready or Degraded change status.
func (r *VirtualNetworkReconciler) emitTransitionEvents(
	vnet *vnetv1alpha1.VirtualNetwork, priorReady, priorDegraded metav1.ConditionStatus,
) {
	if c := meta.FindStatusCondition(vnet.Status.Conditions, "Ready"); c != nil && c.Status != priorReady {
		switch c.Status {
		case metav1.ConditionTrue:
			eventf(r.Recorder, vnet, corev1.EventTypeNormal, EventReady, "Reconcile", "%s", conditionMessage(c))
		case metav1.ConditionFalse:
			eventf(r.Recorder, vnet, corev1.EventTypeWarning, EventNotReady, "Reconcile", "%s", conditionMessage(c))
		}
	}
	if c := meta.FindStatusCondition(vnet.Status.Conditions, "Degraded"); c != nil && c.Status != priorDegraded {
		switch c.Status {
		case metav1.ConditionTrue:
			eventf(r.Recorder, vnet, corev1.EventTypeWarning, EventDegraded, "Reconcile", "%s", conditionMessage(c))
		case metav1.ConditionFalse:
			eventf(r.Recorder, vnet, corev1.EventTypeNormal, EventRecovered, "Reconcile", "%s", conditionMessage(c))
		}
	}
}

func conditionMessage(c *metav1.Condition) string {
	if c.Message != "" {
		return c.Message
	}
	return c.Reason
}

// setReady upserts the Ready condition.
func setReady(vnet *vnetv1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&vnet.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg})
}

// setDegraded upserts the Degraded condition.
func setDegraded(vnet *vnetv1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&vnet.Status.Conditions, metav1.Condition{Type: "Degraded", Status: status, Reason: reason, Message: msg})
}

func conditionStatus(vnet *vnetv1alpha1.VirtualNetwork, t string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(vnet.Status.Conditions, t); c != nil {
		return c.Status
	}
	return metav1.ConditionUnknown
}

// pluralize returns singular for n == 1, else fmt.Sprintf(plural, n).
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return fmt.Sprintf(plural, n)
}

// userJoinPrefix is the prefix of the user join labels, `kube-vnet/net.*`.
const userJoinPrefix = DefaultLabelPrefix + "net."

// isJoinLabel reports whether k is a user `kube-vnet/net.*` or an operator
// `kube-vnet.system/net.*` label. The generator selects on the latter and the
// diagnostics read the former, so a change in either must enqueue.
func isJoinLabel(k string) bool {
	return strings.HasPrefix(k, userJoinPrefix) || strings.HasPrefix(k, LabelSystemNetPrefix)
}

// HasJoinLabel reports whether obj carries a join label (isJoinLabel).
func HasJoinLabel(obj client.Object) bool {
	return len(joinLabelSet(obj)) > 0
}

// joinLabelSet extracts obj's join labels (isJoinLabel).
func joinLabelSet(obj client.Object) map[string]string {
	out := map[string]string{}
	if obj == nil {
		return out
	}
	for k, v := range obj.GetLabels() {
		if isJoinLabel(k) {
			out[k] = v
		}
	}
	return out
}

// JoinLabelChangedPredicate is the change-based pod predicate for the
// VirtualNetworkReconciler. On Update it fires only when the join-label set
// or the resolution marker changed: each reconcile lists every pod, and
// nothing in it depends on pod status, so status churn must not enqueue.
//
// The resolved-generation annotation matters because for a pod whose
// membership is denied, resolution writes only that annotation and no stamp.
// Without it, a vnet reconcile that raced the pod's resolution would leave
// the InvalidJoiners diagnostic stale until the periodic requeue.
//
// Create/Delete/Generic fire for any pod carrying a join label.
func JoinLabelChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return HasJoinLabel(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return HasJoinLabel(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !maps.Equal(joinLabelSet(e.ObjectOld), joinLabelSet(e.ObjectNew)) ||
				resolvedGeneration(e.ObjectOld) != resolvedGeneration(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool { return HasJoinLabel(e.Object) },
	}
}

func resolvedGeneration(obj client.Object) string {
	if obj == nil {
		return ""
	}
	return obj.GetAnnotations()[AnnotationResolvedGeneration]
}

// SetupWithManager wires the watches: VirtualNetwork (primary), Pods (join-label
// changes, old and new labels), managed NetworkPolicies (drift) and
// Namespaces. Bindings need no watch: this reconcile reads only the stamps
// resolution derives from them, and a stamp change is a pod event.
func (r *VirtualNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	podPredicate := JoinLabelChangedPredicate()

	policyPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[LabelManagedBy] == LabelManagedByValue
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&vnetv1alpha1.VirtualNetwork{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToVnets),
			builder.WithPredicates(podPredicate),
		).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToVNet),
			builder.WithPredicates(policyPredicate),
		).
		// Namespace managed-ness gates this reconcile twice: the home namespace
		// decides whether the vnet is served at all, and each member's namespace
		// decides whether its pods count. A disabled home namespace ends the
		// reconcile without a requeue, so only this watch brings the vnet back
		// when it is re-enabled. See ADR 0044.
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.nsToVnets),
		).
		Complete(r)
}

// nsToVnets maps a Namespace event to the vnets whose reconcile could change:
// those living in it (home managed-ness) and those admitting it (member
// managed-ness, and the `allowedNamespaces.selector` labels this event may have
// just changed). Vnets are few, so a full List is cheap.
func (r *VirtualNetworkReconciler) nsToVnets(ctx context.Context, obj client.Object) []reconcile.Request {
	var vnets vnetv1alpha1.VirtualNetworkList
	if err := r.List(ctx, &vnets); err != nil {
		return nil
	}
	ns := obj.GetName()
	var out []reconcile.Request
	for i := range vnets.Items {
		v := &vnets.Items[i]
		// PermitsForVnet admits the home namespace too.
		if ok, err := PermitsForVnet(ctx, r.Client, v, ns); err == nil && ok {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: v.Namespace, Name: v.Name},
			})
		}
	}
	return out
}

// podToVnets maps a pod to the vnets its join labels name, user
// (`kube-vnet/net.*`) and operator (`kube-vnet.system/net.*`) alike. On update
// the handler maps the old and the new pod, so removed memberships are seen too.
// A key naming no existing vnet is harmless: its reconcile finds nothing.
func (r *VirtualNetworkReconciler) podToVnets(_ context.Context, obj client.Object) []reconcile.Request {
	var out []reconcile.Request
	for k := range obj.GetLabels() {
		suffix, ok := strings.CutPrefix(k, userJoinPrefix)
		if !ok {
			if suffix, ok = strings.CutPrefix(k, LabelSystemNetPrefix); !ok {
				continue
			}
		}
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: suffix}
		switch homeNS, name, qualified := strings.Cut(suffix, "."); {
		case suffix == SystemVnetCluster && r.OperatorNamespace != "":
			// Bare `cluster` names the singleton in the operator namespace.
			key.Namespace = r.OperatorNamespace
		case qualified:
			key = types.NamespacedName{Namespace: homeNS, Name: name}
		}
		out = append(out, reconcile.Request{NamespacedName: key})
	}
	return out
}

// policyToVNet maps a managed NetworkPolicy event back to its owning VirtualNetwork
// via the kube-vnet.system/network=<homeNS>.<vnet> label.
func (r *VirtualNetworkReconciler) policyToVNet(_ context.Context, obj client.Object) []reconcile.Request {
	homeNS, name, ok := strings.Cut(obj.GetLabels()[LabelNetwork], ".")
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: homeNS, Name: name}}}
}
