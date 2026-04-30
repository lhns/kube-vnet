package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
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

// Condition reasons surfaced on VirtualNetwork.status.conditions.
const (
	ReasonPoliciesGenerated     = "PoliciesGenerated"
	ReasonNoMembers             = "NoMembers"
	ReasonInvalidJoiners        = "InvalidJoiners"
	ReasonHomeNamespaceExcluded = "HomeNamespaceExcluded"
	ReasonApplyFailed           = "ApplyFailed"
	ReasonInvalidName           = "InvalidName"
	ReasonNamespaceNotAllowed   = "NamespaceNotAllowed"
	ReasonNamespaceExcluded     = "NamespaceExcluded"
	ReasonUnknownDirection      = "UnknownDirection"
	ReasonConflictingDirections = "ConflictingDirections"
	ReasonNoIssues              = "NoIssues"
)

// Event reasons (Kubernetes Event.Reason — short, stable, machine-readable).
const (
	EventReady           = "Ready"
	EventNotReady        = "NotReady"
	EventDegraded        = "Degraded"
	EventRecovered       = "Recovered"
	EventApplyFailed     = "ApplyFailed"
	EventPolicyRestored  = "PolicyRestored"
)

// nameRegex enforces DNS-1123 label format on VirtualNetwork names (no dots).
// The CRD also enforces this via x-kubernetes-validations; this is defense in depth.
var nameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// VirtualNetworkReconciler reconciles VirtualNetwork resources into NetworkPolicies.
type VirtualNetworkReconciler struct {
	client.Client
	// APIReader is an uncached reader, used in cases where we just deleted an
	// object and need a strongly-consistent read to make a follow-up decision
	// (e.g. baseline GC must not skip due to a stale cache showing the just-
	// deleted membership policy as present).
	APIReader   client.Reader
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	LabelPrefix string
	NSFilter    *NamespaceFilter
}

// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworks/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the controller-runtime Reconciler interface.
func (r *VirtualNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx).WithValues("vnet", req.NamespacedName)
	start := time.Now()
	defer func() { observeReconcile(start, err) }()

	vnet := &vnetv1alpha1.VirtualNetwork{}
	if err := r.Get(ctx, req.NamespacedName, vnet); err != nil {
		if apierrors.IsNotFound(err) {
			clearMembers(req.Namespace, req.Name)
			return ctrl.Result{}, r.cleanupForDeleted(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}
	if !vnet.DeletionTimestamp.IsZero() {
		clearMembers(vnet.Namespace, vnet.Name)
		return ctrl.Result{}, r.cleanupForDeleted(ctx, vnet.Namespace, vnet.Name)
	}

	// Snapshot prior condition states so we can emit events on transitions.
	priorReady := conditionStatus(vnet, "Ready")
	priorDegraded := conditionStatus(vnet, "Degraded")

	// Defense-in-depth name validation. The CRD CEL rule should already reject names with dots.
	if !nameRegex.MatchString(vnet.Name) {
		setReady(vnet, metav1.ConditionFalse, ReasonInvalidName,
			fmt.Sprintf("name %q is not a DNS-1123 label", vnet.Name))
		setDegraded(vnet, metav1.ConditionTrue, ReasonInvalidName,
			fmt.Sprintf("VirtualNetwork name %q must match %s", vnet.Name, nameRegex.String()))
		_ = r.updateStatus(ctx, vnet, nil, nil)
		r.emitTransitionEvents(vnet, priorReady, priorDegraded)
		return ctrl.Result{}, nil
	}

	// Reject home namespace if it is unmanaged (operator-level exclusion or
	// per-namespace kube-vnet/disabled annotation).
	homeNS, err := r.getNamespace(ctx, vnet.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if homeNS == nil || !r.NSFilter.IsManaged(homeNS) {
		setReady(vnet, metav1.ConditionFalse, ReasonHomeNamespaceExcluded,
			fmt.Sprintf("home namespace %q is excluded by the operator", vnet.Namespace))
		setDegraded(vnet, metav1.ConditionTrue, ReasonHomeNamespaceExcluded,
			fmt.Sprintf("home namespace %q is in the operator excluded list or has kube-vnet/disabled=true", vnet.Namespace))
		_ = r.updateStatus(ctx, vnet, nil, nil)
		r.emitTransitionEvents(vnet, priorReady, priorDegraded)
		// Clean up any policies that may exist from a previous reconcile.
		_ = r.cleanupForDeleted(ctx, vnet.Namespace, vnet.Name)
		return ctrl.Result{}, nil
	}

	members, invalid, err := r.discoverMembers(ctx, vnet)
	if err != nil {
		return ctrl.Result{}, err
	}

	out := Generate(GenerateInput{
		VNet:        vnet,
		LabelPrefix: r.labelPrefix(),
		MembersByNS: members,
	})

	desiredKeys := make(map[string]bool, len(out.Policies))
	policyRefs := make([]vnetv1alpha1.PolicyRef, 0, len(out.Policies))
	for i := range out.Policies {
		p := &out.Policies[i]
		desiredKeys[p.Namespace+"/"+p.Name] = true
		restored, err := r.applyPolicyAndDetectRestore(ctx, p)
		if err != nil {
			logger.Error(err, "apply policy failed", "policy", p.Namespace+"/"+p.Name)
			applyErrors.WithLabelValues(ApplyErrorMembershipPolicy).Inc()
			r.Recorder.Event(vnet, corev1.EventTypeWarning, EventApplyFailed,
				fmt.Sprintf("apply %s/%s: %v", p.Namespace, p.Name, err))
			setReady(vnet, metav1.ConditionFalse, ReasonApplyFailed, err.Error())
			_ = r.updateStatus(ctx, vnet, members, policyRefs)
			r.emitTransitionEvents(vnet, priorReady, priorDegraded)
			return ctrl.Result{}, err
		}
		if restored {
			r.Recorder.Event(vnet, corev1.EventTypeWarning, EventPolicyRestored,
				fmt.Sprintf("recreated previously-deleted policy %s/%s", p.Namespace, p.Name))
		}
		policyRefs = append(policyRefs, vnetv1alpha1.PolicyRef{Namespace: p.Namespace, Name: p.Name})
	}

	// Ensure baseline in every managed namespace that has members.
	for ns := range members {
		nsObj, err := r.getNamespace(ctx, ns)
		if err != nil {
			return ctrl.Result{}, err
		}
		if nsObj == nil || !r.NSFilter.IsManaged(nsObj) {
			continue
		}
		baseline := DesiredBaseline(ns)
		restored, err := r.applyPolicyAndDetectRestore(ctx, baseline)
		if err != nil {
			logger.Error(err, "apply baseline failed", "namespace", ns)
			applyErrors.WithLabelValues(ApplyErrorBaseline).Inc()
			r.Recorder.Event(vnet, corev1.EventTypeWarning, EventApplyFailed,
				fmt.Sprintf("apply baseline in %s: %v", ns, err))
			return ctrl.Result{}, err
		}
		if restored {
			r.Recorder.Event(vnet, corev1.EventTypeWarning, EventPolicyRestored,
				fmt.Sprintf("recreated previously-deleted baseline in %s", ns))
		}
	}

	if err := r.deleteStale(ctx, vnet, desiredKeys); err != nil {
		return ctrl.Result{}, err
	}

	if len(invalid) > 0 {
		setDegraded(vnet, metav1.ConditionTrue, ReasonInvalidJoiners,
			fmt.Sprintf("%d invalid joiner(s): %s", len(invalid), summarizeInvalid(invalid)))
	} else {
		setDegraded(vnet, metav1.ConditionFalse, ReasonNoIssues, "")
	}
	if len(out.Policies) == 0 {
		setReady(vnet, metav1.ConditionTrue, ReasonNoMembers, "no pods are joining this VirtualNetwork")
	} else {
		setReady(vnet, metav1.ConditionTrue, ReasonPoliciesGenerated,
			fmt.Sprintf("%d NetworkPolic(y|ies) across %d namespace(s)", len(out.Policies), len(members)))
	}

	if err := r.updateStatus(ctx, vnet, members, policyRefs); err != nil {
		return ctrl.Result{}, err
	}
	r.emitTransitionEvents(vnet, priorReady, priorDegraded)

	totalMembers := 0
	for _, byForm := range members {
		seen := map[string]struct{}{}
		for _, byDir := range byForm {
			for _, pods := range byDir {
				for _, p := range pods {
					seen[p] = struct{}{}
				}
			}
		}
		totalMembers += len(seen)
	}
	setMembers(vnet.Namespace, vnet.Name, totalMembers)

	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// getNamespace fetches a Namespace via the cached client. Returns (nil, nil) if not found.
func (r *VirtualNetworkReconciler) getNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ns, nil
}

// labelPrefix returns the configured label prefix or the default, normalized to end in "/".
func (r *VirtualNetworkReconciler) labelPrefix() string {
	if r.LabelPrefix == "" {
		return DefaultLabelPrefix
	}
	if !strings.HasSuffix(r.LabelPrefix, "/") {
		return r.LabelPrefix + "/"
	}
	return r.LabelPrefix
}

// permits decides whether pods in `ns` are allowed to join `vnet` per spec.allowedNamespaces.
// The home namespace is always permitted. The Selector path requires fetching the namespace
// to read its labels; this is cached by the controller-runtime informer.
func (r *VirtualNetworkReconciler) permits(ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork, ns string) (bool, error) {
	if ns == vnet.Namespace {
		return true, nil
	}
	sel := vnet.Spec.AllowedNamespaces
	if sel == nil {
		return false, nil
	}
	if sel.All {
		return true, nil
	}
	for _, n := range sel.Names {
		if n == ns {
			return true, nil
		}
	}
	if sel.Selector != nil {
		nsObj := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: ns}, nsObj); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		s, err := metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return false, err
		}
		if s.Matches(labels.Set(nsObj.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

// discoverMembers lists pods cluster-wide and partitions them into the
// generator's MembersByNS shape (namespace → key form → direction → pods).
//
// A pod becomes a member when it carries a join label key recognized for its
// namespace AND the value parses to a non-none Direction AND its namespace
// is operator-managed AND (for foreign namespaces) the vnet's
// allowedNamespaces permits it.
//
// The home namespace accepts both bare and prefixed forms; conflicting
// directions across the two forms surface as InvalidJoiner with reason
// ConflictingDirections. Unknown direction values surface as
// UnknownDirection. Pods in excluded / disabled namespaces surface as
// NamespaceExcluded; pods in non-permitted foreign namespaces as
// NamespaceNotAllowed.
func (r *VirtualNetworkReconciler) discoverMembers(
	ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork,
) (members map[string]map[KeyForm]map[Direction][]string, invalid []InvalidJoiner, err error) {
	members = map[string]map[KeyForm]map[Direction][]string{}
	prefix := r.labelPrefix()
	bareKey := prefix + "net." + vnet.Name
	prefixedKey := prefix + "net." + vnet.Namespace + "." + vnet.Name

	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil, nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]

		// Find which forms the pod carries (and parse their direction values).
		// The home namespace recognizes both forms; foreign namespaces only the
		// prefixed form.
		bareVal, hasBare := "", false
		prefVal, hasPref := "", false
		if p.Namespace == vnet.Namespace {
			bareVal, hasBare = p.Labels[bareKey]
		}
		if v, ok := p.Labels[prefixedKey]; ok {
			prefVal, hasPref = v, true
		}
		if !hasBare && !hasPref {
			continue
		}

		// Parse each present form's direction.
		bareDir, bareOK := DirectionNone, true
		if hasBare {
			bareDir, bareOK = ParseDirection(bareVal)
		}
		prefDir, prefOK := DirectionNone, true
		if hasPref {
			prefDir, prefOK = ParseDirection(prefVal)
		}

		// Unknown direction values surface but don't make the pod a member.
		if !bareOK || !prefOK {
			invalid = append(invalid, InvalidJoiner{
				PodNamespace: p.Namespace,
				PodName:      p.Name,
				Reason:       ReasonUnknownDirection,
			})
			continue
		}

		// In the home namespace, both forms can be present. If they're both
		// effective members and disagree, surface ConflictingDirections.
		if hasBare && hasPref && bareDir != DirectionNone && prefDir != DirectionNone && bareDir != prefDir {
			invalid = append(invalid, InvalidJoiner{
				PodNamespace: p.Namespace,
				PodName:      p.Name,
				Reason:       ReasonConflictingDirections,
			})
			continue
		}

		// If both present but one of them is None, that's also conflicting
		// (the user wrote "kube-vnet/net.X: false" and "kube-vnet/net.<homeNS>.X: both"
		// — ambiguous intent).
		if hasBare && hasPref && (bareDir == DirectionNone) != (prefDir == DirectionNone) {
			invalid = append(invalid, InvalidJoiner{
				PodNamespace: p.Namespace,
				PodName:      p.Name,
				Reason:       ReasonConflictingDirections,
			})
			continue
		}

		// At this point: any present form has a meaningful direction (or both
		// are None — meaning the pod opted out via every form it set).
		if (hasBare && bareDir == DirectionNone) || (hasPref && prefDir == DirectionNone) {
			// "false"/"none" present — explicit non-membership.
			continue
		}

		// Drop pods in unmanaged namespaces.
		ns, err := r.getNamespace(ctx, p.Namespace)
		if err != nil {
			return nil, nil, err
		}
		if ns == nil || !r.NSFilter.IsManaged(ns) {
			invalid = append(invalid, InvalidJoiner{
				PodNamespace: p.Namespace,
				PodName:      p.Name,
				Reason:       ReasonNamespaceExcluded,
			})
			continue
		}

		// Foreign-namespace pod: must be permitted by spec.allowedNamespaces.
		if p.Namespace != vnet.Namespace {
			ok, err := r.permits(ctx, vnet, p.Namespace)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				invalid = append(invalid, InvalidJoiner{
					PodNamespace: p.Namespace,
					PodName:      p.Name,
					Reason:       ReasonNamespaceNotAllowed,
				})
				continue
			}
		}

		// Honored member. Record under each form actually present (so the
		// generator knows which forms to materialize policies for).
		if members[p.Namespace] == nil {
			members[p.Namespace] = map[KeyForm]map[Direction][]string{}
		}
		if hasBare {
			if members[p.Namespace][KeyBare] == nil {
				members[p.Namespace][KeyBare] = map[Direction][]string{}
			}
			members[p.Namespace][KeyBare][bareDir] = append(members[p.Namespace][KeyBare][bareDir], p.Name)
		}
		if hasPref {
			if members[p.Namespace][KeyPrefixed] == nil {
				members[p.Namespace][KeyPrefixed] = map[Direction][]string{}
			}
			members[p.Namespace][KeyPrefixed][prefDir] = append(members[p.Namespace][KeyPrefixed][prefDir], p.Name)
		}
	}

	// Sort pod lists for deterministic output (used by status.members display).
	for ns := range members {
		for form := range members[ns] {
			for dir := range members[ns][form] {
				sort.Strings(members[ns][form][dir])
			}
		}
	}
	return members, invalid, nil
}

// applyPolicy server-side-applies a NetworkPolicy with the operator's field manager.
func (r *VirtualNetworkReconciler) applyPolicy(ctx context.Context, p *networkingv1.NetworkPolicy) error {
	p.SetResourceVersion("")
	return r.Patch(ctx, p, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership)
}

// applyPolicyAndDetectRestore server-side-applies a policy and returns whether the
// apply effectively re-created a previously-existing operator-managed policy. The
// caller can then emit a PolicyRestored event so deletion-then-recreation is
// visible to operators (drift correction is otherwise silent — see ADR 0019).
//
// The "re-create" signal is: the policy was absent immediately before our apply.
// We use the uncached APIReader so the staleness window of the informer cache
// doesn't make us miss a real deletion.
func (r *VirtualNetworkReconciler) applyPolicyAndDetectRestore(
	ctx context.Context, p *networkingv1.NetworkPolicy,
) (restored bool, err error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	pre := &networkingv1.NetworkPolicy{}
	getErr := reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Name}, pre)
	wasAbsent := apierrors.IsNotFound(getErr)
	if getErr != nil && !wasAbsent {
		return false, getErr
	}
	if err := r.applyPolicy(ctx, p); err != nil {
		return false, err
	}
	return wasAbsent, nil
}

// deleteStale removes operator-managed policies for this vnet that aren't in
// the desired set, and GCs the baseline in any namespace that became empty.
func (r *VirtualNetworkReconciler) deleteStale(
	ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork, desired map[string]bool,
) error {
	netID := vnet.Namespace + "." + vnet.Name
	var existing networkingv1.NetworkPolicyList
	if err := r.List(ctx, &existing, client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelNetwork:   netID,
	}); err != nil {
		return err
	}
	emptied := map[string]struct{}{}
	for i := range existing.Items {
		p := &existing.Items[i]
		if desired[p.Namespace+"/"+p.Name] {
			continue
		}
		emptied[p.Namespace] = struct{}{}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for ns := range emptied {
		if err := r.gcBaselineIfEmpty(ctx, ns); err != nil {
			return err
		}
	}
	return nil
}

// cleanupForDeleted removes all operator-managed policies for a deleted VirtualNetwork
// and garbage-collects the baseline in any namespace that no longer has any
// operator-managed membership policy.
func (r *VirtualNetworkReconciler) cleanupForDeleted(ctx context.Context, ns, name string) error {
	netID := ns + "." + name
	var policies networkingv1.NetworkPolicyList
	if err := r.List(ctx, &policies, client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelNetwork:   netID,
	}); err != nil {
		return err
	}
	touchedNS := map[string]struct{}{}
	for i := range policies.Items {
		p := &policies.Items[i]
		touchedNS[p.Namespace] = struct{}{}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for nsName := range touchedNS {
		if err := r.gcBaselineIfEmpty(ctx, nsName); err != nil {
			return err
		}
	}
	return nil
}

// gcBaselineIfEmpty deletes the kube-vnet-default-deny baseline in ns if there
// are no operator-managed membership policies left there. The baseline only
// exists to backstop kube-vnet's membership policies; once the last membership
// policy in a namespace is gone, the baseline serves no purpose and should not
// remain to silently isolate workloads.
func (r *VirtualNetworkReconciler) gcBaselineIfEmpty(ctx context.Context, ns string) error {
	// Use the uncached reader: we may have just deleted membership policies
	// and need a strongly-consistent count.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var pols networkingv1.NetworkPolicyList
	if err := reader.List(ctx, &pols,
		client.InNamespace(ns),
		client.MatchingLabels{
			LabelManagedBy: LabelManagedByValue,
			LabelRole:      LabelRoleMembership,
		},
	); err != nil {
		return err
	}
	if len(pols.Items) > 0 {
		return nil
	}
	bp := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := r.Delete(ctx, bp); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// updateStatus writes status fields via the subresource.
//
// `members` is the generator's nested shape (namespace → form → direction →
// pods). For status display we collapse it to a flat per-namespace pod list
// (deduplicated; pods that appear under multiple forms / directions count
// once).
func (r *VirtualNetworkReconciler) updateStatus(
	ctx context.Context,
	vnet *vnetv1alpha1.VirtualNetwork,
	members map[string]map[KeyForm]map[Direction][]string,
	policies []vnetv1alpha1.PolicyRef,
) error {
	out := make([]vnetv1alpha1.NamespaceMembers, 0, len(members))
	for ns, byForm := range members {
		seen := map[string]struct{}{}
		for _, byDir := range byForm {
			for _, pods := range byDir {
				for _, p := range pods {
					seen[p] = struct{}{}
				}
			}
		}
		pods := make([]string, 0, len(seen))
		for p := range seen {
			pods = append(pods, p)
		}
		sort.Strings(pods)
		out = append(out, vnetv1alpha1.NamespaceMembers{Namespace: ns, Pods: pods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	vnet.Status.Members = out
	vnet.Status.GeneratedPolicies = policies
	vnet.Status.ObservedGeneration = vnet.Generation
	return r.Status().Update(ctx, vnet)
}

// emitTransitionEvents emits Kubernetes Events when Ready or Degraded change status.
func (r *VirtualNetworkReconciler) emitTransitionEvents(
	vnet *vnetv1alpha1.VirtualNetwork, priorReady, priorDegraded metav1.ConditionStatus,
) {
	if r.Recorder == nil {
		return
	}
	curReady := conditionStatus(vnet, "Ready")
	if curReady != priorReady {
		c := findCondition(vnet, "Ready")
		switch curReady {
		case metav1.ConditionTrue:
			r.Recorder.Event(vnet, corev1.EventTypeNormal, EventReady, conditionMessage(c))
		case metav1.ConditionFalse:
			r.Recorder.Event(vnet, corev1.EventTypeWarning, EventNotReady, conditionMessage(c))
		}
	}
	curDegraded := conditionStatus(vnet, "Degraded")
	if curDegraded != priorDegraded {
		c := findCondition(vnet, "Degraded")
		switch curDegraded {
		case metav1.ConditionTrue:
			r.Recorder.Event(vnet, corev1.EventTypeWarning, EventDegraded, conditionMessage(c))
		case metav1.ConditionFalse:
			r.Recorder.Event(vnet, corev1.EventTypeNormal, EventRecovered, conditionMessage(c))
		}
	}
}

// setReady upserts the Ready condition.
func setReady(vnet *vnetv1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(vnet, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg})
}

// setDegraded upserts the Degraded condition.
func setDegraded(vnet *vnetv1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(vnet, metav1.Condition{Type: "Degraded", Status: status, Reason: reason, Message: msg})
}

func upsertCondition(vnet *vnetv1alpha1.VirtualNetwork, c metav1.Condition) {
	now := metav1.Now()
	for i, existing := range vnet.Status.Conditions {
		if existing.Type == c.Type {
			if existing.Status != c.Status {
				c.LastTransitionTime = now
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			vnet.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = now
	vnet.Status.Conditions = append(vnet.Status.Conditions, c)
}

func conditionStatus(vnet *vnetv1alpha1.VirtualNetwork, t string) metav1.ConditionStatus {
	if c := findCondition(vnet, t); c != nil {
		return c.Status
	}
	return metav1.ConditionUnknown
}

func findCondition(vnet *vnetv1alpha1.VirtualNetwork, t string) *metav1.Condition {
	for i := range vnet.Status.Conditions {
		if vnet.Status.Conditions[i].Type == t {
			return &vnet.Status.Conditions[i]
		}
	}
	return nil
}

func conditionMessage(c *metav1.Condition) string {
	if c == nil {
		return ""
	}
	if c.Message != "" {
		return c.Message
	}
	return c.Reason
}

func summarizeInvalid(in []InvalidJoiner) string {
	if len(in) == 0 {
		return ""
	}
	const max = 3
	parts := make([]string, 0, max+1)
	for i, j := range in {
		if i >= max {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(in)-max))
			break
		}
		parts = append(parts, j.PodNamespace+"/"+j.PodName)
	}
	return strings.Join(parts, ", ")
}

// SetupWithManager wires watches: VirtualNetwork (primary), Pod (label-prefix predicate
// + handler.Funcs to see old+new on Update), NetworkPolicy (managed-by predicate, drift).
func (r *VirtualNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	prefix := r.labelPrefix()
	keyPrefix := prefix + "net."

	// Predicate fires when *either* the old or new object has any join label.
	hasJoinLabel := func(obj client.Object) bool {
		if obj == nil {
			return false
		}
		for k := range obj.GetLabels() {
			if strings.HasPrefix(k, keyPrefix) {
				return true
			}
		}
		return false
	}
	podPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return hasJoinLabel(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return hasJoinLabel(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasJoinLabel(e.ObjectOld) || hasJoinLabel(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool { return hasJoinLabel(e.Object) },
	}

	policyPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[LabelManagedBy] == LabelManagedByValue
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&vnetv1alpha1.VirtualNetwork{}).
		Watches(
			&corev1.Pod{},
			r.podEventHandler(keyPrefix),
			builder.WithPredicates(podPredicate),
		).
		Watches(
			&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToVNet),
			builder.WithPredicates(policyPredicate),
		).
		Complete(r)
}

// podEventHandler returns a handler.Funcs that enqueues the union of vnets
// referenced by a pod's old and new labels. This catches both adds and removes
// of memberships without any in-memory cache.
func (r *VirtualNetworkReconciler) podEventHandler(keyPrefix string) handler.EventHandler {
	enqueue := func(q workqueue.TypedRateLimitingInterface[reconcile.Request], podNS string, lbls map[string]string) {
		for k := range lbls {
			if !strings.HasPrefix(k, keyPrefix) {
				continue
			}
			rest := strings.TrimPrefix(k, keyPrefix)
			parts := strings.SplitN(rest, ".", 2)
			switch len(parts) {
			case 1:
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: podNS, Name: parts[0],
				}})
			case 2:
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: parts[0], Name: parts[1],
				}})
			}
		}
	}
	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, e.Object.GetNamespace(), e.Object.GetLabels())
		},
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if e.ObjectOld != nil {
				enqueue(q, e.ObjectOld.GetNamespace(), e.ObjectOld.GetLabels())
			}
			if e.ObjectNew != nil {
				enqueue(q, e.ObjectNew.GetNamespace(), e.ObjectNew.GetLabels())
			}
		},
		DeleteFunc: func(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, e.Object.GetNamespace(), e.Object.GetLabels())
		},
		GenericFunc: func(_ context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, e.Object.GetNamespace(), e.Object.GetLabels())
		},
	}
}

// policyToVNet maps a managed NetworkPolicy event back to its owning VirtualNetwork
// via the kube-vnet/network=<homeNS>.<vnet> label.
func (r *VirtualNetworkReconciler) policyToVNet(_ context.Context, obj client.Object) []reconcile.Request {
	v := obj.GetLabels()[LabelNetwork]
	if v == "" {
		return nil
	}
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: parts[0], Name: parts[1]}}}
}
