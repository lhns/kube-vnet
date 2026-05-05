package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

const (
	// LabelManagedBy marks operator-owned NetworkPolicy resources.
	LabelManagedBy = "kube-vnet/managed-by"
	// LabelManagedByValue is the value of LabelManagedBy on operator-owned policies.
	LabelManagedByValue = "kube-vnet"
	// LabelNetwork identifies the VirtualNetwork that owns a policy: "<homeNS>.<vnet>".
	LabelNetwork = "kube-vnet/network"
	// LabelRole distinguishes membership policies from the baseline.
	LabelRole = "kube-vnet/role"
	// LabelRoleMembership marks per-VirtualNetwork membership policies.
	LabelRoleMembership = "membership"
	// LabelRoleBaseline marks the namespace default-deny baseline.
	LabelRoleBaseline = "baseline"

	// NamespaceMetadataNameLabel is the well-known label every namespace carries
	// (k8s >=1.22) — used for namespaceSelector matching.
	NamespaceMetadataNameLabel = "kubernetes.io/metadata.name"

	// DNSAppLabelKey/Value match the standard CoreDNS pod label.
	DNSAppLabelKey   = "k8s-app"
	DNSAppLabelValue = "kube-dns"
	// KubeSystemNamespace is the well-known namespace housing CoreDNS.
	KubeSystemNamespace = "kube-system"

	// DefaultLabelPrefix is the default label key prefix for the join labels and
	// operator-internal labels. Configurable at runtime.
	DefaultLabelPrefix = "kube-vnet/"

	// FieldManager is the server-side-apply field manager name used by the operator.
	FieldManager = "kube-vnet"
)

// Direction is the per-pod direction of a vnet membership. Set as the value
// of a join label (or via a VirtualNetworkBinding's spec.direction).
//
// Valid values: `both`, `ingress`, `egress`, `none`. Any other value (including
// the legacy `true`/`false`/empty-string aliases that earlier ADRs honored)
// is rejected by ParseDirection. The direction-value VAP shipped via the
// chart also rejects them at admission. Callers that hit ok=false from
// ParseDirection should surface the value as an InvalidJoiner with reason
// UnknownDirection. See ADR 0030 and the ADR 0021 2026-05-05 addendum.
type Direction string

const (
	DirectionBoth    Direction = "both"
	DirectionIngress Direction = "ingress"
	DirectionEgress  Direction = "egress"
	DirectionNone    Direction = "none"
)

// ParseDirection normalizes a label value to a Direction. Returns ok=false
// for any value other than the four enum constants. The legacy aliases
// `true`, `false`, and the empty string are no longer accepted (dropped per
// ADR 0030; see the ADR 0021 2026-05-05 addendum). The direction-value VAP
// rejects them at admission too.
func ParseDirection(value string) (Direction, bool) {
	switch value {
	case "both":
		return DirectionBoth, true
	case "ingress":
		return DirectionIngress, true
	case "egress":
		return DirectionEgress, true
	case "none":
		return DirectionNone, true
	}
	return DirectionNone, false
}

// KeyForm distinguishes the bare and namespace-prefixed forms of the join
// label key. The bare form is only valid in the VirtualNetwork's home
// namespace; the prefixed form works in any namespace.
type KeyForm int

const (
	KeyBare     KeyForm = 0
	KeyPrefixed KeyForm = 1
)

// InvalidJoiner records a pod that was rejected as a member (wrong namespace,
// unknown direction, conflicting forms in home, etc.).
type InvalidJoiner struct {
	PodNamespace string
	PodName      string
	Reason       string
}

// GenerateInput is the pure input to the policy generator.
//
// MembersByNS is keyed by namespace, then by KeyForm (which form of the join
// label the pod uses — only the home namespace ever has KeyBare entries),
// then by Direction. The leaf is the list of pod names (informational —
// generated selectors match by label, not by name).
//
// BindingsByNS is keyed by namespace, with the list of label-free
// VirtualNetworkBinding-driven members in that namespace. Each binding
// contributes one membership policy (with the binding's podSelector
// verbatim) AND one peer entry in every other policy's peer rules.
type GenerateInput struct {
	VNet         *vnetv1alpha1.VirtualNetwork
	LabelPrefix  string
	MembersByNS  map[string]map[KeyForm]map[Direction][]string
	BindingsByNS map[string][]BindingSpec
}

// BindingSpec is the generator's view of one VirtualNetworkBinding (already
// scoped to bindings that target the current VirtualNetwork and live in a
// namespace permitted by spec.allowedNamespaces).
type BindingSpec struct {
	// Name is the binding's metadata.name. Used to build the per-binding
	// policy name.
	Name string
	// Direction is the parsed direction the binding establishes.
	Direction Direction
	// PodSelector is the binding's spec.podSelector (verbatim).
	PodSelector metav1.LabelSelector
}

// GenerateOutput holds the desired NetworkPolicies.
type GenerateOutput struct {
	Policies []networkingv1.NetworkPolicy
}

// JoinLabelKey returns the label key a pod sets to join the given VirtualNetwork
// from inPodNS. For pods in the home namespace the bare form
// "<prefix>net.<vnet>" works. The prefixed form
// "<prefix>net.<homeNS>.<vnet>" works in any namespace including the home one.
//
// This is the user-facing input scheme. The resolution controller stamps the
// equivalent under the operator-output prefix; see SystemLabelKey.
func JoinLabelKey(prefix, homeNS, vnet, inPodNS string) string {
	if inPodNS == homeNS {
		return prefix + "net." + vnet
	}
	return prefix + "net." + homeNS + "." + vnet
}

// JoinLabelKeyByForm returns the join label key for a (vnet, namespace, form).
// The bare form is only meaningful in the home namespace; callers shouldn't
// produce KeyBare entries for foreign namespaces.
func JoinLabelKeyByForm(prefix, homeNS, vnet string, form KeyForm) string {
	if form == KeyBare {
		return prefix + "net." + vnet
	}
	return prefix + "net." + homeNS + "." + vnet
}

// SystemLabelKeyByForm is like JoinLabelKeyByForm but always emits keys under
// the operator's `kube-vnet.system/net.` prefix. Used by the policy generator
// to build NetworkPolicy podSelectors that match the labels stamped by the
// resolution controller. See ADR 0030.
//
// System vnets ("namespace", "cluster") always use bare form regardless of
// the form argument — resolution stamps them as bare for every pod.
func SystemLabelKeyByForm(homeNS, vnet string, form KeyForm) string {
	if isSystemVnetName(vnet) || form == KeyBare {
		return LabelSystemNetPrefix + vnet
	}
	return LabelSystemNetPrefix + homeNS + "." + vnet
}

// PolicyName returns the deterministic NetworkPolicy name for the bare-form
// membership policy of (vnet, homeNS). Shape: `kube-vnet.<vnet>-<8hex>`.
// The prefix is dot-separated to keep visual structure clean even when
// vnet/namespace names contain dashes (e.g. `netpol-demo`).
//
// Bare-form policies only exist in the vnet's home namespace; the policy's
// `metadata.namespace` is implicitly homeNS, so it doesn't appear in the name.
func PolicyName(vnet, homeNS string) string {
	return truncatePolicyName(fmt.Sprintf("kube-vnet.%s-%s",
		vnet, policyHash("bare", homeNS, vnet)))
}

// PolicyNameFor returns the deterministic NetworkPolicy name for a given
// (vnet, homeNS, key form). Shapes:
//
//	bare:     kube-vnet.<vnet>-<8hex>
//	prefixed: kube-vnet.<homeNS>.<vnet>-<8hex>
//
// The prefixed shape mirrors the join-label key `kube-vnet/net.<homeNS>.<vnet>`,
// using `.` as a separator (forbidden inside DNS-1123 labels, so unambiguous).
// The hash disambiguates internal-component boundaries within either shape.
//
// All receiver-capable direction classes (`both` and `ingress`) share a
// single self-policy per (ns, form). `egress`-only members produce no
// self-policy. See ADR 0021 (Addendum) for the consolidation rationale.
func PolicyNameFor(vnet, homeNS string, form KeyForm) string {
	if form == KeyBare {
		return PolicyName(vnet, homeNS)
	}
	return truncatePolicyName(fmt.Sprintf("kube-vnet.%s.%s-%s",
		homeNS, vnet, policyHash("prefixed", homeNS, vnet)))
}

// policyHash returns an 8-hex-char identity hash for collision-safe naming.
// Inputs are joined with `\x00` — forbidden in DNS-1123 labels and Kubernetes
// resource names — so distinct (parts...) tuples always produce distinct
// pre-hash strings. SHA-256 is overkill for collision avoidance at this size
// but matches what `truncatePolicyName` already uses for overflow disambiguation.
//
// This is an *identity* hash (inputs are class + identifying fields), not a
// content hash of the rendered NetworkPolicy spec. Names stay stable across
// membership churn so the reconciler's server-side apply patches the existing
// object instead of churning delete+create.
func policyHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:4])
}

func truncatePolicyName(name string) string {
	const max = 253
	if len(name) <= max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:4])
	keep := max - len(suffix)
	if keep < 0 {
		keep = 0
	}
	return name[:keep] + suffix
}

// Direction value helpers for selector LabelSelectorRequirement values.
var (
	// selfValuesReceiver matches pods that ACCEPT ingress: `both`,
	// `ingress`, plus the legacy `true` alias (which means `both`).
	// Used as the policy's own podSelector In-values for the single
	// merged self-policy per (ns, form). Egress-only members are not
	// included — they don't accept ingress, so they don't need a self-
	// policy at all.
	selfValuesReceiver = []string{string(DirectionBoth), string(DirectionIngress)}

	// peerInitiatorValues matches peers that can INITIATE traffic
	// (potential sources of ingress to me). Used in ingress.from.
	peerInitiatorValues = []string{string(DirectionBoth), string(DirectionEgress)}
)

// hasReceiver reports whether the (form, direction-map) tuple has any
// pod that accepts ingress (`both` or `ingress`). Used to decide whether
// to emit a self-policy at all.
func hasReceiver(byDir map[Direction][]string) bool {
	return len(byDir[DirectionBoth]) > 0 || len(byDir[DirectionIngress]) > 0
}

// hasInitiator reports whether the (form, direction-map) tuple has any
// pod that can initiate traffic (egress-capable: both or egress). Used
// to decide whether to emit a peer entry that other pods' ingress.from
// rules will reference.
func hasInitiator(byDir map[Direction][]string) bool {
	return len(byDir[DirectionBoth]) > 0 || len(byDir[DirectionEgress]) > 0
}

// dirHasIngress reports whether a binding's direction should produce a
// self-policy. Bindings with `egress` or `none` direction get no self-
// policy (they accept no ingress).
func dirHasIngress(d Direction) bool { return d == DirectionBoth || d == DirectionIngress }

// Generate returns the desired NetworkPolicy set for a VirtualNetwork.
//
// Membership policies are ingress-only (PolicyTypes: [Ingress]). The operator
// never restricts egress (ADR 0025: ingress-isolation-only model). A pod
// joining a vnet still resolves DNS, reaches the apiserver, talks to the
// internet — exactly the pre-membership posture for egress.
//
// Per (namespace, key-form), up to two policies are produced — one for
// receiver-capable members (direction `both` and `ingress`) and one for
// initiator-only members (direction `egress`) iff that pod itself needs
// no ingress restrictions… actually direction `egress` produces NO
// self-policy because such pods don't accept ingress and we don't restrict
// their egress. So in practice: at most one self-policy per (ns, form)
// for the receiver-capable members. The home namespace can have entries
// under both KeyBare and KeyPrefixed when both forms are in use,
// doubling the per-namespace cap to 2.
//
// Owner references are set only on policies in the home namespace
// (Kubernetes rejects cross-namespace owner refs).
func Generate(in GenerateInput) GenerateOutput {
	prefix := in.LabelPrefix
	if prefix == "" {
		prefix = DefaultLabelPrefix
	}
	vnet := in.VNet
	homeNS := vnet.Namespace
	netID := homeNS + "." + vnet.Name

	// Sort namespaces for deterministic output. The union of label-driven
	// and binding-driven membership defines the namespaces that participate.
	nsSet := map[string]struct{}{}
	for ns, byForm := range in.MembersByNS {
		for _, byDir := range byForm {
			for _, pods := range byDir {
				if len(pods) > 0 {
					nsSet[ns] = struct{}{}
					break
				}
			}
		}
	}
	for ns, bs := range in.BindingsByNS {
		for _, b := range bs {
			if b.Direction != DirectionNone {
				nsSet[ns] = struct{}{}
				break
			}
		}
	}
	memberNamespaces := make([]string, 0, len(nsSet))
	for ns := range nsSet {
		memberNamespaces = append(memberNamespaces, ns)
	}
	sort.Strings(memberNamespaces)

	out := GenerateOutput{}
	if len(memberNamespaces) == 0 {
		return out
	}

	// Pre-build peer rules.
	//
	// For each peer namespace + form combination that has at least one
	// initiator, we'll emit one ingress.from peer rule. Same for receivers
	// in egress.to.
	type peerKey struct {
		ns   string
		form KeyForm
	}

	// Collect (peerNS, peerForm) tuples that can initiate (sources of ingress).
	// Membership policies are ingress-only now, so we don't track receivers.
	initiators := []peerKey{}
	for _, peerNS := range memberNamespaces {
		byForm := in.MembersByNS[peerNS]
		for _, form := range []KeyForm{KeyBare, KeyPrefixed} {
			byDir, ok := byForm[form]
			if !ok {
				continue
			}
			if hasInitiator(byDir) {
				initiators = append(initiators, peerKey{peerNS, form})
			}
		}
	}

	makePeer := func(pk peerKey, values []string) networkingv1.NetworkPolicyPeer {
		key := SystemLabelKeyByForm(homeNS, vnet.Name, pk.form)
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{NamespaceMetadataNameLabel: pk.ns},
			},
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      key,
					Operator: metav1.LabelSelectorOpIn,
					Values:   values,
				}},
			},
		}
	}

	peerFroms := make([]networkingv1.NetworkPolicyPeer, 0, len(initiators))
	for _, pk := range initiators {
		peerFroms = append(peerFroms, makePeer(pk, peerInitiatorValues))
	}

	// Bindings contribute additional peer entries (initiators only — we don't
	// restrict egress, so receivers don't need to appear in any peer list).
	// Bindings with DirectionNone are skipped.
	type bindingPeer struct {
		ns       string
		selector metav1.LabelSelector
	}
	bindingInitiators := []bindingPeer{}
	bindingNSes := make([]string, 0, len(in.BindingsByNS))
	for ns := range in.BindingsByNS {
		bindingNSes = append(bindingNSes, ns)
	}
	sort.Strings(bindingNSes)
	for _, ns := range bindingNSes {
		bs := in.BindingsByNS[ns]
		// Stable order by binding name within a namespace.
		sortedBs := make([]BindingSpec, len(bs))
		copy(sortedBs, bs)
		sort.Slice(sortedBs, func(i, j int) bool { return sortedBs[i].Name < sortedBs[j].Name })
		for _, b := range sortedBs {
			if b.Direction == DirectionNone {
				continue
			}
			if b.Direction == DirectionBoth || b.Direction == DirectionEgress {
				bindingInitiators = append(bindingInitiators, bindingPeer{ns, b.PodSelector})
			}
		}
	}
	makeBindingPeer := func(bp bindingPeer) networkingv1.NetworkPolicyPeer {
		sel := bp.selector
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{NamespaceMetadataNameLabel: bp.ns},
			},
			PodSelector: &sel,
		}
	}
	for _, bp := range bindingInitiators {
		peerFroms = append(peerFroms, makeBindingPeer(bp))
	}

	// Note: membership policies are strictly ingress-only. The operator never
	// adds egress restrictions; egress (DNS, the apiserver, the public
	// internet, other namespaces) is unrestricted from the operator's
	// perspective. See ADR 0025.

	policies := []networkingv1.NetworkPolicy{}
	for _, ns := range memberNamespaces {
		byForm := in.MembersByNS[ns]
		for _, form := range []KeyForm{KeyBare, KeyPrefixed} {
			byDir, ok := byForm[form]
			if !ok {
				continue
			}
			selectorKey := SystemLabelKeyByForm(homeNS, vnet.Name, form)
			// One self-policy per (ns, form), selecting all receiver-capable
			// members (`both` and `ingress`, plus the legacy `true` alias).
			// Direction `egress`-only members don't get a self-policy:
			// they accept no ingress and we don't restrict egress. They
			// still appear in *other* pods' ingress.from peer lists via
			// peerInitiatorValues.
			if !hasReceiver(byDir) {
				continue
			}
			policy := networkingv1.NetworkPolicy{
				TypeMeta: metav1.TypeMeta{
					APIVersion: networkingv1.SchemeGroupVersion.String(),
					Kind:       "NetworkPolicy",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      PolicyNameFor(vnet.Name, homeNS, form),
					Labels: map[string]string{
						LabelManagedBy: LabelManagedByValue,
						LabelNetwork:   netID,
						LabelRole:      LabelRoleMembership,
					},
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{{
							Key:      selectorKey,
							Operator: metav1.LabelSelectorOpIn,
							Values:   selfValuesReceiver,
						}},
					},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				},
			}
			if len(peerFroms) > 0 {
				policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: peerFroms}}
			}

			if ns == homeNS {
				policy.OwnerReferences = []metav1.OwnerReference{{
					APIVersion:         vnetv1alpha1.GroupVersion.String(),
					Kind:               "VirtualNetwork",
					Name:               vnet.Name,
					UID:                vnet.UID,
					Controller:         ptrTrue(),
					BlockOwnerDeletion: ptrTrue(),
				}}
			}
			policies = append(policies, policy)
		}
	}

	// Per-binding policies. One policy per binding (in the binding's own
	// namespace). The policy's podSelector is the binding's verbatim
	// podSelector; ingress/egress shape follows the binding's direction.
	for _, ns := range bindingNSes {
		bs := in.BindingsByNS[ns]
		sortedBs := make([]BindingSpec, len(bs))
		copy(sortedBs, bs)
		sort.Slice(sortedBs, func(i, j int) bool { return sortedBs[i].Name < sortedBs[j].Name })
		for _, b := range sortedBs {
			// Same logic as label-driven members: bindings whose direction
			// doesn't accept ingress (`egress` or `none`) get no self-policy.
			if !dirHasIngress(b.Direction) {
				continue
			}
			policy := networkingv1.NetworkPolicy{
				TypeMeta: metav1.TypeMeta{
					APIVersion: networkingv1.SchemeGroupVersion.String(),
					Kind:       "NetworkPolicy",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      BindingPolicyName(homeNS, vnet.Name, ns, b.Name),
					Labels: map[string]string{
						LabelManagedBy: LabelManagedByValue,
						LabelNetwork:   netID,
						LabelRole:      LabelRoleMembership,
						LabelBinding:   b.Name,
					},
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: b.PodSelector,
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				},
			}
			if len(peerFroms) > 0 {
				policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: peerFroms}}
			}
			policies = append(policies, policy)
		}
	}
	out.Policies = policies
	return out
}

// LabelBinding marks per-VirtualNetworkBinding membership policies with the
// binding's name (for traceability and easy GC of policies whose binding
// vanished).
const LabelBinding = "kube-vnet/binding"

// BindingPolicyName returns the deterministic policy name for a
// VirtualNetworkBinding-driven membership policy. Shape:
//
//	kube-vnet.<homeNS>.<vnet>.b.<binding>-<8hex>
//
// The `.b.` marker distinguishes binding policies from prefixed-form
// membership (`kube-vnet.<homeNS>.<vnet>-<hash>`). The hash disambiguates
// against pathological inputs where `.` parsing could otherwise be ambiguous.
func BindingPolicyName(homeNS, vnet, bindingNS, binding string) string {
	return truncatePolicyName(fmt.Sprintf("kube-vnet.%s.%s.b.%s-%s",
		homeNS, vnet, binding,
		policyHash("binding", homeNS, vnet, bindingNS, binding)))
}

func ptrTrue() *bool { b := true; return &b }
