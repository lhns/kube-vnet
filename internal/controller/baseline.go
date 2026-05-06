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
// elideFor entries are vnet name suffixes (the part after
// `kube-vnet.system/net.`). They follow the same canonicalization rule the
// resolution controller uses for pod-input labels (CanonicalSuffix), with
// `ns` substituted for the pod's NS:
//
//   - `cluster`            → kube-vnet.system/net.<operatorNS>.cluster (lone exception)
//   - `namespace`          → kube-vnet.system/net.<ns>.namespace       (per-NS)
//   - bare user vnet name  → kube-vnet.system/net.<ns>.<name>          (per-NS)
//   - `<homeNS>.<name>`    → kube-vnet.system/net.<homeNS>.<name>       (FQ pass-through)
//
// Callers that want "no kube-vnet objects in this namespace" must check
// IsManaged separately; the disabled-namespaces path bypasses
// DesiredBaseline entirely.
func DesiredBaseline(ns, operatorNS string, elideFor []string) *networkingv1.NetworkPolicy {
	matchExpressions := make([]metav1.LabelSelectorRequirement, 0, len(elideFor))
	for _, suffix := range elideFor {
		matchExpressions = append(matchExpressions, metav1.LabelSelectorRequirement{
			Key:      LabelSystemNetPrefix + CanonicalSuffix(suffix, ns, operatorNS),
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
