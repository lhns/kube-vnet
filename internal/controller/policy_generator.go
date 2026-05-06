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
// Valid values: `both`, `ingress`, `egress`, `none`, plus the four `default-*`
// variants (`default-both`, `default-ingress`, `default-egress`, `default-none`)
// used at baseline tiers to mark a value as override-able by lower tiers. See
// ADR 0031. Bare values are enforced (no override permitted); `default-*`
// values are advisory.
//
// Pod-tier values (pod label and `VirtualNetworkBinding.spec.direction`)
// accept only the bare four — the `default-*` prefix is meaningless at the
// leaf tier and is rejected at admission (CRD CEL) and at runtime (label
// parser). Any other value (including the legacy `true`/`false`/empty-string
// aliases that earlier ADRs honored) is rejected by ParseDirection. The
// direction-value VAP shipped via the chart also rejects unknown values at
// admission. Callers that hit ok=false from ParseDirection should surface the
// value as an InvalidJoiner with reason UnknownDirection. See ADR 0030, the
// ADR 0021 2026-05-05 addendum, and ADR 0031.
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

// ParseDirection normalizes a label value to a Direction. Returns ok=false
// for any value other than the eight enum constants. The legacy aliases
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
	case "default-both":
		return DirectionDefaultBoth, true
	case "default-ingress":
		return DirectionDefaultIngress, true
	case "default-egress":
		return DirectionDefaultEgress, true
	case "default-none":
		return DirectionDefaultNone, true
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
// values pass through unchanged. The final emitted direction (label stamped
// onto pods) is always bare; the default-* prefix is consumed during
// resolution to compute override-permission.
func (d Direction) Bare() Direction {
	switch d {
	case DirectionDefaultBoth:
		return DirectionBoth
	case DirectionDefaultIngress:
		return DirectionIngress
	case DirectionDefaultEgress:
		return DirectionEgress
	case DirectionDefaultNone:
		return DirectionNone
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
// MembersByNS is keyed by namespace, then by Direction. Per ADR 0033, every
// member is selected via a single canonical FQ system label key
// (`kube-vnet.system/net.<homeNS>.<vnet>`), regardless of whether the pod's
// stamp came from a user label, a `VirtualNetworkBinding`, or a baseline.
// Bindings no longer produce a separate generator-input axis; they stamp the
// same canonical system label as everything else and are picked up here as
// regular members.
type GenerateInput struct {
	VNet        *vnetv1alpha1.VirtualNetwork
	LabelPrefix string
	MembersByNS map[string]map[Direction][]string
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
// This is the user-facing input scheme (per ADR 0022 — both forms are
// accepted on inputs). The resolution controller normalizes both to the
// canonical FQ form on the operator-output side; see SystemLabelKey.
func JoinLabelKey(prefix, homeNS, vnet, inPodNS string) string {
	if inPodNS == homeNS {
		return prefix + "net." + vnet
	}
	return prefix + "net." + homeNS + "." + vnet
}

// SystemLabelKey returns the canonical operator-stamped label key for a
// vnet, in the form `kube-vnet.system/net.<homeNS>.<vnet>`. Per ADR 0033,
// this is the only shape used on the output side — pod stamps and policy
// selectors both use it. System vnets follow the same rule (homeNS is the
// pod's NS for `namespace`; the operator's release NS for `cluster`).
func SystemLabelKey(homeNS, vnet string) string {
	return LabelSystemNetPrefix + homeNS + "." + vnet
}

// PolicyName returns the deterministic NetworkPolicy name. Per ADR 0033 the
// shape is uniformly `kube-vnet.<homeNS>.<vnet>-<8hex>` for every vnet
// (user and system). The 8-hex suffix is a SHA-256-based identity hash that
// disambiguates against name collisions; see ADR 0011 for the truncate-and-
// hash overflow handler.
func PolicyName(vnet, homeNS string) string {
	if vnet == SystemVnetCluster {
		// Cluster is the cluster-wide singleton; bare-named everywhere
		// per ADR 0033 amendment. Hash inputs use empty homeNS so the
		// name is stable regardless of which NS the operator runs in.
		return truncatePolicyName(fmt.Sprintf("kube-vnet.%s-%s",
			SystemVnetCluster, policyHash("membership", "", SystemVnetCluster)))
	}
	return truncatePolicyName(fmt.Sprintf("kube-vnet.%s.%s-%s",
		homeNS, vnet, policyHash("membership", homeNS, vnet)))
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
// Per ADR 0033, exactly one membership policy is produced per
// (vnet, member-namespace) — namely each namespace where at least one pod
// has direction `both` or `ingress` for the canonical system label
// `kube-vnet.system/net.<homeNS>.<vnet>`. Egress-only members produce no
// self-policy (they accept no ingress and we don't restrict egress) but
// still appear in other namespaces' policies as `from:` peers.
//
// `VirtualNetworkBinding`-driven members are handled identically to label-
// driven members: the resolution controller stamps the canonical system
// label on selected pods, and they show up in MembersByNS like everything
// else. No per-binding policy is emitted.
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

	// Member namespaces: any NS that has at least one pod with non-empty
	// direction-bucket. Sorted for deterministic output.
	nsSet := map[string]struct{}{}
	for ns, byDir := range in.MembersByNS {
		for _, pods := range byDir {
			if len(pods) > 0 {
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
	out.Policies = policies
	return out
}

func ptrTrue() *bool { b := true; return &b }
