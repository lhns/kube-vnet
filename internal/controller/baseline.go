package controller

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BaselinePolicyName is the name of the operator-managed default-deny baseline.
// Per-namespace singleton — namespace scoping handles uniqueness, so no hash
// suffix is needed. Distinct from every membership/binding name (those all
// add `-<vnet>...`), so user-chosen vnet/ns combinations cannot collide here.
const BaselinePolicyName = "kube-vnet"

// IsolationMode is the per-namespace ingress-isolation level.
//
// Egress is intentionally not part of this enum: kube-vnet does not provide
// an egress-isolation story (per the discussion in ADR 0025). Pods retain
// unrestricted egress unless a user-managed NetworkPolicy or cluster-level
// egress firewall says otherwise.
type IsolationMode string

const (
	// IsolationNone — no baseline ingress restriction. Equivalent to "the
	// operator's baseline doesn't exist for this namespace."
	IsolationNone IsolationMode = "none"

	// IsolationNamespace — baseline allows ingress only from pods in the
	// same namespace. Cross-namespace ingress is denied unless an additional
	// policy (e.g. a vnet membership policy) allows it.
	IsolationNamespace IsolationMode = "namespace"

	// IsolationPod — baseline denies all ingress. Only vnet membership
	// policies (or other user-managed policies) grant ingress allows.
	IsolationPod IsolationMode = "pod"
)

// ParseIsolationMode normalizes a string value into an IsolationMode.
// Returns ok=false for unrecognized values; the parsed mode is IsolationNone
// in that case.
func ParseIsolationMode(value string) (IsolationMode, bool) {
	switch value {
	case "none", "":
		return IsolationNone, true
	case "namespace":
		return IsolationNamespace, true
	case "pod":
		return IsolationPod, true
	}
	return IsolationNone, false
}

// DesiredBaseline returns the baseline NetworkPolicy for a namespace given
// the desired ingress-isolation mode. Always returns a non-nil policy for
// every IsolationMode — the baseline expresses the namespace's "outer
// boundary" for ingress, which is well-defined even in IsolationNone (where
// the boundary is "everywhere"). See ADR 0029.
//
// The baseline carries `policyTypes: [Ingress]` only — egress is never
// restricted by kube-vnet. See ADR 0025.
//
// Callers that want "no kube-vnet objects in this namespace" must check
// IsManaged separately; the disabled-namespaces path bypasses
// DesiredBaseline entirely.
func DesiredBaseline(ns string, mode IsolationMode) *networkingv1.NetworkPolicy {
	policy := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: networkingv1.SchemeGroupVersion.String(),
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      BaselinePolicyName,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
				LabelRole:      LabelRoleBaseline,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	switch mode {
	case IsolationNone:
		// One empty ingress rule = "allow all sources on all ports" per
		// the K8s NetworkPolicy spec ("An empty NetworkPolicyIngressRule
		// matches all traffic"). Note: a NetworkPolicyPeer cannot itself
		// be empty (the apiserver rejects `from: [{}]`); the right idiom
		// is to leave both `from` and `ports` unset on the rule.
		// Functionally indistinguishable from "no policy" — present so
		// the namespace's mode is visible in `kubectl get netpol` and so
		// the additive union with vnet membership policies stays
		// "everywhere" (model: mode is the namespace's outer boundary).
		policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
	case IsolationNamespace:
		policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{NamespaceMetadataNameLabel: ns},
				},
			}},
		}}
	case IsolationPod:
		// No allow rules — every ingress is denied. Membership policies are
		// what grant per-vnet allows.
		policy.Spec.Ingress = nil
	}
	return policy
}
