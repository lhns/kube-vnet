package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Operator-owned label keys live under `kube-vnet.system/` per ADR 0037, the
// same convention that applies to operator-stamped pod labels
// (`kube-vnet.system/net.*`). The user-surface prefix `kube-vnet/` is reserved
// for user inputs (join labels, the `kube-vnet/disabled` annotation).
const (
	// LabelK8sManagedBy is the Kubernetes recommended managed-by label,
	// stamped (with LabelManagedByValue) on every operator-emitted resource
	// for ecosystem convention only.
	//
	// Never select, sweep, or gate any operation on it: it is user-writable
	// and cannot be VAP-protected (Helm stamps it cluster-wide), so a user
	// could add it to a third-party policy and a sweep would delete an object
	// we don't own. LabelManagedBy (admission-protected, ADR 0037) is the
	// sole ownership signal.
	LabelK8sManagedBy = "app.kubernetes.io/managed-by"

	// LabelManagedBy marks operator-owned NetworkPolicies and the
	// operator-created system VirtualNetworks (`namespace`, `cluster`).
	LabelManagedBy = "kube-vnet.system/managed-by"
	// LabelManagedByValue is the value of LabelManagedBy on operator-owned objects.
	LabelManagedByValue = "kube-vnet"
	// LabelNetwork identifies the VirtualNetwork that owns a policy: "<homeNS>.<vnet>".
	LabelNetwork = "kube-vnet.system/network"
	// LabelRole distinguishes membership, baseline and external-allow policies.
	LabelRole = "kube-vnet.system/role"
	// LabelRoleMembership marks per-VirtualNetwork membership policies.
	LabelRoleMembership = "membership"
	// LabelRoleBaseline marks the namespace default-deny baseline.
	LabelRoleBaseline = "baseline"
	// LabelRoleExternalAllow marks the additive allow-from-an-ipBlock
	// policies: Service-source (ADR 0038), host-port (ADR 0040) and
	// apiserver-reachable (ADR 0041).
	LabelRoleExternalAllow = "external-allow"
	// LabelSystemHostPortPrefix prefixes the pod stamps
	// `kube-vnet.system/host-port.<port>.<protocol>=true` that the host-port
	// policies select (ADR 0040).
	LabelSystemHostPortPrefix = "kube-vnet.system/host-port."
	// LabelSource names what an external-allow policy was emitted for (ADR
	// 0039): `svc-<service>`, `host-<port>-<protocol>` or
	// `apiserver-<service>`. Reconcilers dispatch on LabelSourceKind, never
	// on parsing this.
	LabelSource = "kube-vnet.system/source"
	// LabelSourceKind is the source kind of an external-allow policy, so
	// each reconciler sweeps only its own (a Service named `host-8080-tcp`
	// is not a host-port source).
	LabelSourceKind = "kube-vnet.system/source-kind"

	// Values of LabelSourceKind, and the third segment of external-allow
	// policy names (`kube-vnet.ext.<kind>.*`).
	LabelSourceKindService   = "svc"
	LabelSourceKindHost      = "host"
	LabelSourceKindApiserver = "apiserver"

	// NamespaceMetadataNameLabel is the well-known label every namespace carries
	// (k8s >=1.22) — used for namespaceSelector matching.
	NamespaceMetadataNameLabel = "kubernetes.io/metadata.name"

	// DefaultLabelPrefix is the prefix of user-facing keys: join labels
	// (`kube-vnet/net.*`) and annotations.
	DefaultLabelPrefix = "kube-vnet/"

	// FieldManager is the server-side-apply field manager name used by the operator.
	FieldManager = "kube-vnet"

	// Policy name kinds, the second segment of
	// `kube-vnet.<kind>.<identity>-<8hex>` (ADR 0039).
	PolicyKindMembership = "mem"
	PolicyKindExternal   = "ext"
)

// Direction is the per-pod direction of a vnet membership. Set as the value
// of a join label (or via a VirtualNetworkBinding's spec.direction).
//
// Valid values: `both`, `ingress`, `egress`, `none`, plus the four `default-*`
// variants (`default-both`, `default-ingress`, `default-egress`, `default-none`)
// used at baseline tiers to mark a value as override-able by lower tiers. See
// ADR 0031. Bare values are enforced (no override permitted); `default-*`
// values are advisory.
//
// Pod-tier values (pod label and `VirtualNetworkBinding.spec.direction`)
// accept only the bare four (ParseBareDirection); `default-*` is meaningless
// at the leaf tier. See ADR 0030 and ADR 0031.
type Direction string

const (
	DirectionBoth    Direction = "both"
	DirectionIngress Direction = "ingress"
	DirectionEgress  Direction = "egress"
	DirectionNone    Direction = "none"

	// default-* variants are valid at baseline tiers only (ADR 0031).
	DirectionDefaultBoth    Direction = "default-both"
	DirectionDefaultIngress Direction = "default-ingress"
	DirectionDefaultEgress  Direction = "default-egress"
	DirectionDefaultNone    Direction = "default-none"
)

// ParseDirection parses a label value into a Direction. Returns ok=false for
// any value other than the eight Direction constants.
func ParseDirection(value string) (Direction, bool) {
	switch d := Direction(value); d {
	case DirectionBoth, DirectionIngress, DirectionEgress, DirectionNone,
		DirectionDefaultBoth, DirectionDefaultIngress, DirectionDefaultEgress, DirectionDefaultNone:
		return d, true
	}
	return DirectionNone, false
}

// ParseBareDirection accepts only the four bare values (rejects default-*).
// Used at the pod tier (label parsing, VirtualNetworkBinding direction) where
// the override-permission concept doesn't apply.
func ParseBareDirection(value string) (Direction, bool) {
	d, ok := ParseDirection(value)
	if !ok || d.IsDefault() {
		return DirectionNone, false
	}
	return d, true
}

// IsDefault reports whether d is one of the override-permitted (default-*)
// variants.
func (d Direction) IsDefault() bool {
	switch d {
	case DirectionDefaultBoth, DirectionDefaultIngress, DirectionDefaultEgress, DirectionDefaultNone:
		return true
	}
	return false
}

// Bare strips the default-* prefix, returning the bare equivalent. Bare
// values pass through unchanged.
func (d Direction) Bare() Direction {
	if d.IsDefault() {
		return d[len("default-"):]
	}
	return d
}

// InvalidJoiner records a pod that was rejected as a member (wrong namespace,
// unknown direction, etc.).
type InvalidJoiner struct {
	PodNamespace string
	PodName      string
	Reason       string
}

// GenerateInput is the pure input to the policy generator.
//
// MembersByNS is keyed by namespace, then by Direction. Every member is
// selected by the one canonical system label (SystemLabelKey, ADR 0033),
// whichever source — join label, binding, or baseline — stamped it.
type GenerateInput struct {
	VNet        *vnetv1alpha1.VirtualNetwork
	MembersByNS map[string]map[Direction][]string
}

// GenerateOutput holds the desired NetworkPolicies.
type GenerateOutput struct {
	Policies []networkingv1.NetworkPolicy
}

// SystemLabelKey returns the canonical operator-stamped label key for a
// vnet. Per ADR 0033, the form is `kube-vnet.system/net.<homeNS>.<vnet>`;
// per the ADR 0033 Amendment, the cluster system vnet collapses to bare
// `kube-vnet.system/net.cluster` (cluster is the cluster-wide singleton —
// the prefix carries no information). The resolution controller stamps
// pods with the same shape, so the policy generator's selectors line up.
func SystemLabelKey(homeNS, vnet string) string {
	if vnet == SystemVnetCluster {
		return LabelSystemNetPrefix + SystemVnetCluster
	}
	return LabelSystemNetPrefix + homeNS + "." + vnet
}

// PolicyName returns the deterministic membership NetworkPolicy name. Per
// ADR 0039 the shape is `kube-vnet.mem.<homeNS>.<vnet>-<8hex>` for
// namespaced vnets, or `kube-vnet.mem.cluster-<8hex>` for the cluster
// singleton (per ADR 0033 amendment — the cluster vnet retains a bare
// identity inside the mem.<…> prefix because cluster has no homeNS). The
// 8-hex suffix is a SHA-256-based identity hash that disambiguates against
// name collisions; see ADR 0011 for the truncate-and-hash overflow handler.
func PolicyName(vnet, homeNS string) string {
	if vnet == SystemVnetCluster {
		return truncatePolicyName(fmt.Sprintf("kube-vnet.%s.%s-%s",
			PolicyKindMembership, SystemVnetCluster, policyHash("membership", "", SystemVnetCluster)))
	}
	return truncatePolicyName(fmt.Sprintf("kube-vnet.%s.%s.%s-%s",
		PolicyKindMembership, homeNS, vnet, policyHash("membership", homeNS, vnet)))
}

// policyHash returns an 8-hex-char identity hash for collision-safe naming.
// Inputs are joined with `\x00`, which no name contains, so distinct tuples
// hash distinct strings. It hashes identity, not content, so names stay
// stable across membership churn and applies patch rather than recreate.
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

// SourceLabelValue builds the `kube-vnet.system/source` value identifying the
// object a generated policy came from, bounded to the 63-character label-value
// limit: ADR 0011's truncate-and-hash applied to a label value.
//
// Callers must use this both for writing the label and for any selector that
// queries it (see serviceSource.deleteByServiceKey); if the two disagree, deletes
// silently match nothing and leave policies orphaned. The hash covers
// namespace/name so two truncated-to-identical names stay distinguishable.
//
// Values that already fit are returned unchanged, so existing policies keep
// their labels and nothing is rewritten on upgrade.
func SourceLabelValue(prefix, namespace, name string) string {
	const max = 63
	full := prefix + name
	if len(full) <= max {
		return full
	}
	suffix := "-" + policyHash("source", namespace, name)
	keep := max - len(prefix) - len(suffix)
	if keep < 0 {
		keep = 0
	}
	return prefix + name[:keep] + suffix
}

// Direction values for the membership policy's label selectors.
var (
	// selfValuesReceiver selects the pods a membership policy applies to:
	// those that accept ingress. Egress-only members need no policy.
	selfValuesReceiver = []string{string(DirectionBoth), string(DirectionIngress)}

	// peerInitiatorValues selects the peers that may initiate traffic;
	// used in ingress.from.
	peerInitiatorValues = []string{string(DirectionBoth), string(DirectionEgress)}
)

// hasReceiver reports whether any pod in byDir accepts ingress, i.e. whether
// its namespace needs a membership policy.
func hasReceiver(byDir map[Direction][]string) bool {
	return len(byDir[DirectionBoth]) > 0 || len(byDir[DirectionIngress]) > 0
}

// hasInitiator reports whether any pod in byDir can initiate traffic, i.e.
// whether its namespace appears as an ingress.from peer.
func hasInitiator(byDir map[Direction][]string) bool {
	return len(byDir[DirectionBoth]) > 0 || len(byDir[DirectionEgress]) > 0
}

// Generate returns the desired NetworkPolicy set for a VirtualNetwork.
//
// Membership policies are ingress-only; the operator never restricts egress
// (ADR 0025).
//
// One policy is produced per member namespace that has at least one pod
// accepting ingress (`both` or `ingress`). Egress-only members get no policy
// but still appear as `from:` peers in the other namespaces' policies.
//
// Owner references are set only on policies in the home namespace
// (Kubernetes rejects cross-namespace owner refs).
func Generate(in GenerateInput) GenerateOutput {
	vnet := in.VNet
	homeNS := vnet.Namespace
	netID := homeNS + "." + vnet.Name

	// Member namespaces: any NS that has at least one pod with non-empty
	// direction-bucket. Sorted for deterministic output.
	var memberNamespaces []string
	for ns, byDir := range in.MembersByNS {
		for _, pods := range byDir {
			if len(pods) > 0 {
				memberNamespaces = append(memberNamespaces, ns)
				break
			}
		}
	}
	slices.Sort(memberNamespaces)

	out := GenerateOutput{}
	if len(memberNamespaces) == 0 {
		return out
	}

	// Pre-build peer rules: one ingress.from peer per namespace that has at
	// least one initiator (`both` or `egress`). All peers share the same
	// canonical FQ system-label selector key.
	selectorKey := SystemLabelKey(homeNS, vnet.Name)
	peerFroms := make([]networkingv1.NetworkPolicyPeer, 0, len(memberNamespaces))
	for _, peerNS := range memberNamespaces {
		if !hasInitiator(in.MembersByNS[peerNS]) {
			continue
		}
		peerFroms = append(peerFroms, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{NamespaceMetadataNameLabel: peerNS},
			},
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      selectorKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   peerInitiatorValues,
				}},
			},
		})
	}

	// One membership policy per receiver-bearing namespace.
	policies := []networkingv1.NetworkPolicy{}
	for _, ns := range memberNamespaces {
		byDir := in.MembersByNS[ns]
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
				Name:      PolicyName(vnet.Name, homeNS),
				Labels: map[string]string{
					LabelManagedBy:    LabelManagedByValue,
					LabelK8sManagedBy: LabelManagedByValue,
					LabelNetwork:      netID,
					LabelRole:         LabelRoleMembership,
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
				Controller:         new(true),
				BlockOwnerDeletion: new(true),
			}}
		}
		policies = append(policies, policy)
	}
	out.Policies = policies
	return out
}
