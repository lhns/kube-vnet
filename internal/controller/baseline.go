package controller

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BaselinePolicyName is the name of the operator-managed default-deny
// baseline policy. Per ADR 0039 the shape is the kind-prefixed
// `kube-vnet.base` (literal — per-namespace singleton, no identity to
// hash).
const BaselinePolicyName = "kube-vnet.base"

// DesiredBaseline returns the deny-all baseline NetworkPolicy for a managed
// namespace. Per ADR 0030, the baseline is uniformly deny-all ingress
// (`policyTypes: [Ingress]`, no allow rules) selecting every pod in the
// namespace. Per ADR 0035, there is no elide-list exemption: the previous
// `--elide-baseline-for` mechanism added a `NotIn` matchExpression to the
// `podSelector` to skip cluster-receiver pods, but per NetworkPolicy union
// semantics that had no observable effect — the baseline contributes only
// deny-all (zero allows), and a pod's effective ingress is determined by the
// allows from any selecting membership policy. Removing the elide knob
// preserves connectivity exactly.
//
// Callers that want "no kube-vnet objects in this namespace" must check
// IsManaged separately; the disabled-namespaces path bypasses
// DesiredBaseline entirely.
func DesiredBaseline(ns string) *networkingv1.NetworkPolicy {
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
			PodSelector: metav1.LabelSelector{}, // selects all pods in ns
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// No Ingress rules — deny-all.
		},
	}
}
