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
	ReasonResolutionConflict    = "ResolutionConflict"
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
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=kube-vnet.lhns.de,resources=virtualnetworkbindings/status,verbs=get;update;patch
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
	// per-namespace kube-vnet/disabled annotation). System vnets are exempt:
	// the cluster system vnet's home is the operator namespace, which is
	// implicitly disabled in cmd/main.go as a privilege boundary, and per-
	// namespace system vnets in user-disabled namespaces still need to exist
	// so resolution works the moment the namespace becomes managed again.
	// The system-vnet VAP keeps the kube-vnet/system label honest.
	isSystem := vnet.Labels[LabelSystem] == LabelSystemValue
	homeNS, err := r.getNamespace(ctx, vnet.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isSystem && (homeNS == nil || !r.NSFilter.IsManaged(homeNS)) {
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

	// Baseline lifecycle is owned by NamespaceReconciler (ADR 0023, kept under
	// ADR 0030). Per ADR 0030 + ADR 0035 the baseline is uniformly deny-all
	// selecting every pod; the vnet reconciler doesn't touch it.

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
	for _, byDir := range members {
		seen := map[string]struct{}{}
		for _, pods := range byDir {
			for _, p := range pods {
				seen[p] = struct{}{}
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
// generator's MembersByNS shape (namespace → direction → pods). Per ADR 0033
// the membership signal is the canonical FQ system label
// `kube-vnet.system/net.<vnet.Namespace>.<vnet.Name>`, populated by the
// resolution controller from any source (user labels, bindings, baselines).
//
// Diagnostic scan on user-prefix labels surfaces InvalidJoiner reasons
// (UnknownDirection, NamespaceExcluded, NamespaceNotAllowed). Per ADR 0033
// `ConflictingDirections` is gone — the resolver canonicalizes both bare
// and prefixed user labels to the same VnetKey at stamp time and intersects
// any disagreements; cross-source disagreements surface separately as
// `ResolutionConflict` via `ResolutionResult.Conflicts`.
func (r *VirtualNetworkReconciler) discoverMembers(
	ctx context.Context, vnet *vnetv1alpha1.VirtualNetwork,
) (members map[string]map[Direction][]string, invalid []InvalidJoiner, err error) {
	members = map[string]map[Direction][]string{}
	sysKey := SystemLabelKey(vnet.Namespace, vnet.Name)

	userPrefix := r.labelPrefix()
	userBareKey := userPrefix + "net." + vnet.Name
	userPrefixedKey := userPrefix + "net." + vnet.Namespace + "." + vnet.Name
	systemVnet := isSystemVnetName(vnet.Name)

	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil, nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]

		// Diagnostic scan on USER-prefix labels (separate from membership;
		// surfaces InvalidJoiner reasons). Bare-form is only valid in the
		// home NS for user vnets, or in any managed NS for system vnets.
		userBareVal, hasUserBare := "", false
		userPrefVal, hasUserPref := "", false
		if p.Namespace == vnet.Namespace || systemVnet {
			userBareVal, hasUserBare = p.Labels[userBareKey]
		}
		if !systemVnet {
			if v, ok := p.Labels[userPrefixedKey]; ok {
				userPrefVal, hasUserPref = v, true
			}
		}
		if hasUserBare {
			if _, ok := ParseBareDirection(userBareVal); !ok {
				invalid = append(invalid, InvalidJoiner{
					PodNamespace: p.Namespace, PodName: p.Name, Reason: ReasonUnknownDirection,
				})
				continue
			}
		}
		if hasUserPref {
			if _, ok := ParseBareDirection(userPrefVal); !ok {
				invalid = append(invalid, InvalidJoiner{
					PodNamespace: p.Namespace, PodName: p.Name, Reason: ReasonUnknownDirection,
				})
				continue
			}
		}
		if hasUserBare || hasUserPref {
			ns, err := r.getNamespace(ctx, p.Namespace)
			if err != nil {
				return nil, nil, err
			}
			if ns == nil || !r.NSFilter.IsManaged(ns) {
				invalid = append(invalid, InvalidJoiner{
					PodNamespace: p.Namespace, PodName: p.Name, Reason: ReasonNamespaceExcluded,
				})
				continue
			}
			if p.Namespace != vnet.Namespace && !systemVnet {
				ok, err := r.permits(ctx, vnet, p.Namespace)
				if err != nil {
					return nil, nil, err
				}
				if !ok {
					invalid = append(invalid, InvalidJoiner{
						PodNamespace: p.Namespace, PodName: p.Name, Reason: ReasonNamespaceNotAllowed,
					})
					continue
				}
			}
		}

		// Fail-closed during the resolution race window: a pod with no
		// resolved-generation annotation hasn't been processed by the
		// resolution controller yet. Exclude it from policy generation
		// rather than risk emitting policies based on partial state.
		if p.Annotations[AnnotationResolvedGeneration] == "" {
			continue
		}

		// Membership is determined by the canonical FQ system label only.
		sysVal, hasSys := p.Labels[sysKey]
		if !hasSys {
			continue
		}
		dir, ok := ParseBareDirection(sysVal)
		if !ok || dir == DirectionNone {
			continue
		}
		if members[p.Namespace] == nil {
			members[p.Namespace] = map[Direction][]string{}
		}
		members[p.Namespace][dir] = append(members[p.Namespace][dir], p.Name)
	}

	// Sort pod lists for deterministic output (used by status.members display).
	for ns := range members {
		for dir := range members[ns] {
			sort.Strings(members[ns][dir])
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

// deleteStale removes operator-managed membership policies for this vnet
// that aren't in the desired set. Baseline lifecycle is owned by
// NamespaceReconciler (ADR 0023).
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
	for i := range existing.Items {
		p := &existing.Items[i]
		if desired[p.Namespace+"/"+p.Name] {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// cleanupForDeleted removes all operator-managed membership policies for a
// deleted VirtualNetwork. The baseline is owned by NamespaceReconciler
// independently and isn't tied to any specific vnet's lifecycle; we don't
// touch it here. See ADR 0023 (decoupling) and ADR 0030 (current shape).
func (r *VirtualNetworkReconciler) cleanupForDeleted(ctx context.Context, ns, name string) error {
	netID := ns + "." + name
	var policies networkingv1.NetworkPolicyList
	if err := r.List(ctx, &policies, client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelNetwork:   netID,
	}); err != nil {
		return err
	}
	for i := range policies.Items {
		p := &policies.Items[i]
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// updateStatus writes status fields via the subresource.
//
// `members` is namespace → direction → pods (canonical post-ADR-0033 shape).
// For status display we collapse to a flat per-namespace pod list
// (deduplicated; pods listed under multiple directions count once).
func (r *VirtualNetworkReconciler) updateStatus(
	ctx context.Context,
	vnet *vnetv1alpha1.VirtualNetwork,
	members map[string]map[Direction][]string,
	policies []vnetv1alpha1.PolicyRef,
) error {
	out := make([]vnetv1alpha1.NamespaceMembers, 0, len(members))
	for ns, byDir := range members {
		seen := map[string]struct{}{}
		for _, pods := range byDir {
			for _, p := range pods {
				seen[p] = struct{}{}
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
		// "<ns>/<pod>:<reason>" — surfaces the per-pod failure category on
		// the vnet's Degraded message so a user reading `kubectl describe
		// vnet` sees which pod failed for which specific reason instead of
		// only a flat list of names.
		parts = append(parts, fmt.Sprintf("%s/%s:%s", j.PodNamespace, j.PodName, j.Reason))
	}
	return strings.Join(parts, ", ")
}

// HasJoinLabel reports whether obj carries at least one label key with the
// kube-vnet user-input prefix `<labelPrefix>net.` OR the operator-stamped
// prefix `kube-vnet.system/net.`. Used as the predicate for the
// VirtualNetworkReconciler's pod watch and the JoinLabelDiagnosticReconciler's
// pod watch. The system prefix is included because the generator selects on
// system-stamped labels (ADR 0030), so changes to them must enqueue the
// affected vnet.
func HasJoinLabel(obj client.Object, labelPrefix string) bool {
	if obj == nil {
		return false
	}
	userPrefix := labelPrefix + "net."
	for k := range obj.GetLabels() {
		if strings.HasPrefix(k, userPrefix) || strings.HasPrefix(k, LabelSystemNetPrefix) {
			return true
		}
	}
	return false
}

// JoinLabelPodPredicate returns a predicate that fires when *either* the old
// or new pod object carries any join label.
func JoinLabelPodPredicate(labelPrefix string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return HasJoinLabel(e.Object, labelPrefix) },
		DeleteFunc: func(e event.DeleteEvent) bool { return HasJoinLabel(e.Object, labelPrefix) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return HasJoinLabel(e.ObjectOld, labelPrefix) || HasJoinLabel(e.ObjectNew, labelPrefix)
		},
		GenericFunc: func(e event.GenericEvent) bool { return HasJoinLabel(e.Object, labelPrefix) },
	}
}

// SetupWithManager wires watches: VirtualNetwork (primary), Pod (label-prefix predicate
// + handler.Funcs to see old+new on Update), NetworkPolicy (managed-by predicate, drift).
func (r *VirtualNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	prefix := r.labelPrefix()
	keyPrefix := prefix + "net."

	podPredicate := JoinLabelPodPredicate(prefix)

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
		Watches(
			&vnetv1alpha1.VirtualNetworkBinding{},
			handler.EnqueueRequestsFromMapFunc(r.bindingToVNet),
		).
		Complete(r)
}

// podEventHandler returns a handler.Funcs that enqueues the union of vnets
// referenced by a pod's old and new labels. This catches both adds and removes
// of memberships without any in-memory cache.
//
// Two label prefixes are considered: the user-input prefix (`kube-vnet/net.`)
// authored by users, and the operator-stamped prefix (`kube-vnet.system/net.`)
// written by the resolution controller. The generator selects on the
// system-prefixed labels, so changes to those must also re-enqueue the
// affected vnet (per ADR 0030).
func (r *VirtualNetworkReconciler) podEventHandler(keyPrefix string) handler.EventHandler {
	enqueueOne := func(q workqueue.TypedRateLimitingInterface[reconcile.Request], podNS, suffix string) {
		parts := strings.SplitN(suffix, ".", 2)
		switch len(parts) {
		case 1:
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: podNS, Name: parts[0],
			}})
		case 2:
			// System vnet `cluster` lives in the operator namespace, not in
			// `parts[0]`. We enqueue the parsed namespace anyway — the
			// reconciler tolerates "vnet doesn't exist" by returning early.
			// Same for any prefixed-form key that happens to refer to a
			// non-existent vnet.
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: parts[0], Name: parts[1],
			}})
		}
	}
	enqueue := func(q workqueue.TypedRateLimitingInterface[reconcile.Request], podNS string, lbls map[string]string) {
		for k := range lbls {
			switch {
			case strings.HasPrefix(k, keyPrefix):
				enqueueOne(q, podNS, strings.TrimPrefix(k, keyPrefix))
			case strings.HasPrefix(k, LabelSystemNetPrefix):
				enqueueOne(q, podNS, strings.TrimPrefix(k, LabelSystemNetPrefix))
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

// bindingToVNet maps a VirtualNetworkBinding event back to its referenced
// VirtualNetwork.
func (r *VirtualNetworkReconciler) bindingToVNet(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*vnetv1alpha1.VirtualNetworkBinding)
	if !ok || b.Spec.VirtualNetworkRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: b.Spec.VirtualNetworkRef.Namespace,
		Name:      b.Spec.VirtualNetworkRef.Name,
	}}}
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
