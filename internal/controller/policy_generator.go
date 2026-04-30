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

// InvalidJoiner records a pod that tried to join a VirtualNetwork it cannot reach.
type InvalidJoiner struct {
	PodNamespace string
	PodName      string
	Reason       string
}

// GenerateInput is the pure input to the policy generator.
type GenerateInput struct {
	VNet        *vnetv1alpha1.VirtualNetwork
	LabelPrefix string
	// MembersByNS lists pod names per namespace that are honored members of the VirtualNetwork.
	// Selectors generated below match by label, not by name; pod names exist only for status.
	MembersByNS map[string][]string
}

// GenerateOutput holds the generated NetworkPolicies and any reasons we couldn't fully honor the input.
type GenerateOutput struct {
	Policies []networkingv1.NetworkPolicy
}

// JoinLabelKey returns the label key a pod sets to join the given VirtualNetwork
// from inPodNS. For pods in the VirtualNetwork's home namespace the bare form
// "<prefix>net.<vnet>" is used; for pods in any other namespace the namespace-
// prefixed form "<prefix>net.<homeNS>.<vnet>" is required.
func JoinLabelKey(prefix, homeNS, vnet, inPodNS string) string {
	if inPodNS == homeNS {
		return prefix + "net." + vnet
	}
	return prefix + "net." + homeNS + "." + vnet
}

// PolicyName returns the deterministic NetworkPolicy name for (vnet, ns), truncating
// with a hash suffix if the result exceeds 253 characters (Kubernetes resource name limit).
func PolicyName(vnet, ns string) string {
	const max = 253
	name := fmt.Sprintf("kube-vnet-%s-%s", vnet, ns)
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

// Generate returns the desired NetworkPolicy set for a VirtualNetwork.
//
// One policy is produced per namespace that has members. Each policy:
//   - Selects pods via Exists on the join label appropriate for that namespace.
//   - Allows ingress from peers in every namespace that has members.
//   - Allows egress symmetric to ingress, plus CoreDNS egress.
//
// Owner references are set only for policies in the VirtualNetwork's home namespace,
// since cross-namespace owner references are unsupported by Kubernetes.
func Generate(in GenerateInput) GenerateOutput {
	prefix := in.LabelPrefix
	if prefix == "" {
		prefix = DefaultLabelPrefix
	}
	vnet := in.VNet
	homeNS := vnet.Namespace
	netID := homeNS + "." + vnet.Name

	memberNamespaces := make([]string, 0, len(in.MembersByNS))
	for ns, pods := range in.MembersByNS {
		if len(pods) == 0 {
			continue
		}
		memberNamespaces = append(memberNamespaces, ns)
	}
	sort.Strings(memberNamespaces)

	out := GenerateOutput{}
	if len(memberNamespaces) == 0 {
		return out
	}

	// Pre-build the peer rules — same for every policy of this VirtualNetwork.
	peerFroms := make([]networkingv1.NetworkPolicyPeer, 0, len(memberNamespaces))
	peerTos := make([]networkingv1.NetworkPolicyPeer, 0, len(memberNamespaces))
	for _, peerNS := range memberNamespaces {
		peerKey := JoinLabelKey(prefix, homeNS, vnet.Name, peerNS)
		peer := networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{NamespaceMetadataNameLabel: peerNS},
			},
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      peerKey,
					Operator: metav1.LabelSelectorOpExists,
				}},
			},
		}
		peerFroms = append(peerFroms, peer)
		peerTos = append(peerTos, peer)
	}

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

	policies := make([]networkingv1.NetworkPolicy, 0, len(memberNamespaces))
	for _, ns := range memberNamespaces {
		selectorKey := JoinLabelKey(prefix, homeNS, vnet.Name, ns)
		policy := networkingv1.NetworkPolicy{
			TypeMeta: metav1.TypeMeta{
				APIVersion: networkingv1.SchemeGroupVersion.String(),
				Kind:       "NetworkPolicy",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      PolicyName(vnet.Name, ns),
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
						Operator: metav1.LabelSelectorOpExists,
					}},
				},
				PolicyTypes: []networkingv1.PolicyType{
					networkingv1.PolicyTypeIngress,
					networkingv1.PolicyTypeEgress,
				},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{From: peerFroms}},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{To: peerTos},
					dnsEgress,
				},
			},
		}
		// Owner reference only for same-namespace policies (Kubernetes rejects cross-namespace owner refs).
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
	out.Policies = policies
	return out
}

func ptrTrue() *bool { b := true; return &b }
