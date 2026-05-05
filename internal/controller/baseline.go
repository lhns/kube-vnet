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

// DesiredBaseline returns the deny-all baseline NetworkPolicy for a managed
// namespace. Per ADR 0030, the baseline is always the same shape: deny-all
// ingress (`policyTypes: [Ingress]`, no allow rules) for every pod in the
// namespace EXCEPT pods that are receivers on any vnet listed in elideFor.
// Default operator config sets elideFor to `[cluster]` so the cluster-vnet
// "everyone reachable" pattern doesn't carry a redundant baseline policy.
//
// elideFor entries are vnet keys (label suffixes after `kube-vnet.system/net.`).
// A pod with `kube-vnet.system/net.<vnet>` set to `both` or `ingress` for any
// vnet in elideFor is excluded from the baseline.
//
// Callers that want "no kube-vnet objects in this namespace" must check
// IsManaged separately; the disabled-namespaces path bypasses
// DesiredBaseline entirely.
func DesiredBaseline(ns string, elideFor []string) *networkingv1.NetworkPolicy {
	matchExpressions := make([]metav1.LabelSelectorRequirement, 0, len(elideFor))
	for _, vnet := range elideFor {
		matchExpressions = append(matchExpressions, metav1.LabelSelectorRequirement{
			Key:      LabelSystemNetPrefix + vnet,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   []string{string(DirectionBoth), string(DirectionIngress)},
		})
	}
	return &networkingv1.NetworkPolicy{
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
			PodSelector: metav1.LabelSelector{
				MatchExpressions: matchExpressions,
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// No Ingress rules — deny-all, except for pods excluded by the
			// elide-list above.
		},
	}
}
