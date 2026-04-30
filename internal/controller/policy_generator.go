package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

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
// Aliases honored by ParseDirection:
//
//	"true" / ""      → DirectionBoth   (legacy: key presence meant member)
//	"false"          → DirectionNone
//
// Anything else is rejected by ParseDirection (returns ok=false). Callers
// should surface unknown values as InvalidJoiner with reason UnknownDirection.
type Direction string

const (
	DirectionBoth    Direction = "both"
	DirectionIngress Direction = "ingress"
	DirectionEgress  Direction = "egress"
	DirectionNone    Direction = "none"
)

// ParseDirection normalizes a label value to a Direction. Returns ok=false
// for unrecognized non-empty values; the parsed Direction is DirectionNone
// in that case (not a member).
func ParseDirection(value string) (Direction, bool) {
	switch value {
	case "both", "true", "":
		return DirectionBoth, true
	case "ingress":
		return DirectionIngress, true
	case "egress":
		return DirectionEgress, true
	case "false", "none":
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
type GenerateInput struct {
	VNet        *vnetv1alpha1.VirtualNetwork
	LabelPrefix string
	MembersByNS map[string]map[KeyForm]map[Direction][]string
}

// GenerateOutput holds the desired NetworkPolicies.
type GenerateOutput struct {
	Policies []networkingv1.NetworkPolicy
}

// JoinLabelKey returns the label key a pod sets to join the given VirtualNetwork
// from inPodNS. For pods in the home namespace the bare form
// "<prefix>net.<vnet>" works. The prefixed form
// "<prefix>net.<homeNS>.<vnet>" works in any namespace including the home one.
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

// PolicyName returns the deterministic NetworkPolicy name for (vnet, ns) in
// the bidirectional / bare-form case. Truncated with a hash suffix if the
// result exceeds 253 characters.
//
// For other direction classes or the long-form-in-home case, see
// PolicyNameFor.
func PolicyName(vnet, ns string) string {
	return truncatePolicyName(fmt.Sprintf("kube-vnet-%s-%s", vnet, ns))
}

// PolicyNameFor returns the deterministic NetworkPolicy name for a given
// (vnet, ns, direction class, key form). Preserves the legacy unsuffixed
// name (`kube-vnet-<vnet>-<ns>`) for the bidirectional + bare case so
// existing installs don't see policy renames.
func PolicyNameFor(vnet, ns, homeNS string, dir Direction, form KeyForm) string {
	suffix := ""
	switch dir {
	case DirectionIngress:
		suffix = "-ingress"
	case DirectionEgress:
		suffix = "-egress"
	}
	// Only the home namespace can have both forms; disambiguate the prefixed-
	// form policy with a -prefixed suffix.
	if ns == homeNS && form == KeyPrefixed {
		suffix += "-prefixed"
	}
	return truncatePolicyName(fmt.Sprintf("kube-vnet-%s-%s%s", vnet, ns, suffix))
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
	// selfValues lists the join-label values that match the policy's own
	// pods given the policy's direction class.
	selfValuesBoth    = []string{"true", string(DirectionBoth)}
	selfValuesIngress = []string{string(DirectionIngress)}
	selfValuesEgress  = []string{string(DirectionEgress)}

	// peerInitiatorValues matches peers that can INITIATE traffic
	// (potential sources of ingress to me). Used in ingress.from.
	peerInitiatorValues = []string{"true", string(DirectionBoth), string(DirectionEgress)}

	// peerReceiverValues matches peers that ACCEPT traffic (potential
	// destinations of egress from me). Used in egress.to.
	peerReceiverValues = []string{"true", string(DirectionBoth), string(DirectionIngress)}
)

func selfValuesFor(d Direction) []string {
	switch d {
	case DirectionBoth:
		return selfValuesBoth
	case DirectionIngress:
		return selfValuesIngress
	case DirectionEgress:
		return selfValuesEgress
	}
	return nil
}

// dirHasIngress reports whether a policy for direction d should include
// PolicyTypes: Ingress + ingress allow rules.
func dirHasIngress(d Direction) bool { return d == DirectionBoth || d == DirectionIngress }

// dirHasEgress reports whether a policy for direction d should include
// PolicyTypes: Egress + egress allow rules.
func dirHasEgress(d Direction) bool { return d == DirectionBoth || d == DirectionEgress }

// hasInitiator reports whether the (form, direction-map) tuple has any
// pod that can initiate traffic (egress-capable: both or egress).
func hasInitiator(byDir map[Direction][]string) bool {
	return len(byDir[DirectionBoth]) > 0 || len(byDir[DirectionEgress]) > 0
}

// hasReceiver reports whether the (form, direction-map) tuple has any
// pod that can accept ingress (ingress-capable: both or ingress).
func hasReceiver(byDir map[Direction][]string) bool {
	return len(byDir[DirectionBoth]) > 0 || len(byDir[DirectionIngress]) > 0
}

// Generate returns the desired NetworkPolicy set for a VirtualNetwork.
//
// Up to three policies are produced per (namespace, key-form) pair —
// one per direction class (bidi, ingress-only, egress-only) that has
// at least one member. The home namespace can have entries under both
// KeyBare and KeyPrefixed when both forms are in use, doubling the
// per-namespace cap to 6.
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

	// Sort namespaces for deterministic output.
	memberNamespaces := make([]string, 0, len(in.MembersByNS))
	for ns, byForm := range in.MembersByNS {
		// Skip namespaces with no actual members across any (form, direction).
		any := false
		for _, byDir := range byForm {
			for _, pods := range byDir {
				if len(pods) > 0 {
					any = true
					break
				}
			}
			if any {
				break
			}
		}
		if any {
			memberNamespaces = append(memberNamespaces, ns)
		}
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
	initiators := []peerKey{}
	receivers := []peerKey{}
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
			if hasReceiver(byDir) {
				receivers = append(receivers, peerKey{peerNS, form})
			}
		}
	}

	makePeer := func(pk peerKey, values []string) networkingv1.NetworkPolicyPeer {
		key := JoinLabelKeyByForm(prefix, homeNS, vnet.Name, pk.form)
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
	peerTos := make([]networkingv1.NetworkPolicyPeer, 0, len(receivers))
	for _, pk := range receivers {
		peerTos = append(peerTos, makePeer(pk, peerReceiverValues))
	}

	// DNS egress rule. Included in every membership policy that has
	// PolicyTypes: Egress (i.e. bidi + egress-only). Until commit 2 reshapes
	// the baseline this stays redundant with the baseline's DNS allow, but
	// matters for pods that aren't reached by the baseline (e.g. the
	// ingress-isolation: namespace mode in commit 2 doesn't restrict egress
	// at all, but we still emit it harmlessly).
	dnsPort53UDP := corev1.ProtocolUDP
	dnsPort53TCP := corev1.ProtocolTCP
	port53 := intstr.FromInt(53)
	dnsEgress := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{NamespaceMetadataNameLabel: KubeSystemNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{DNSAppLabelKey: DNSAppLabelValue},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &dnsPort53UDP, Port: &port53},
			{Protocol: &dnsPort53TCP, Port: &port53},
		},
	}

	policies := []networkingv1.NetworkPolicy{}
	for _, ns := range memberNamespaces {
		byForm := in.MembersByNS[ns]
		for _, form := range []KeyForm{KeyBare, KeyPrefixed} {
			byDir, ok := byForm[form]
			if !ok {
				continue
			}
			selectorKey := JoinLabelKeyByForm(prefix, homeNS, vnet.Name, form)
			for _, dir := range []Direction{DirectionBoth, DirectionIngress, DirectionEgress} {
				if len(byDir[dir]) == 0 {
					continue
				}
				policy := networkingv1.NetworkPolicy{
					TypeMeta: metav1.TypeMeta{
						APIVersion: networkingv1.SchemeGroupVersion.String(),
						Kind:       "NetworkPolicy",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      PolicyNameFor(vnet.Name, ns, homeNS, dir, form),
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
								Values:   selfValuesFor(dir),
							}},
						},
					},
				}

				if dirHasIngress(dir) {
					policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
					if len(peerFroms) > 0 {
						policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: peerFroms}}
					}
				}
				if dirHasEgress(dir) {
					policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
					egressRules := []networkingv1.NetworkPolicyEgressRule{}
					if len(peerTos) > 0 {
						egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{To: peerTos})
					}
					egressRules = append(egressRules, dnsEgress)
					policy.Spec.Egress = egressRules
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
	}
	out.Policies = policies
	return out
}

func ptrTrue() *bool { b := true; return &b }
