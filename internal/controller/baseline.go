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

// DesiredBaseline returns the baseline NetworkPolicy for a managed namespace:
// deny-all ingress (`policyTypes: [Ingress]`, no rules) selecting every pod
// (ADR 0030). It has no exemptions: a deny-all policy adds no allows, so
// excluding pods from it would change nothing (ADR 0035). Callers check
// IsManaged first; unmanaged namespaces get no baseline.
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
				LabelManagedBy:    LabelManagedByValue,
				LabelK8sManagedBy: LabelManagedByValue,
				LabelRole:         LabelRoleBaseline,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // selects all pods in ns
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// No Ingress rules — deny-all.
		},
	}
}
