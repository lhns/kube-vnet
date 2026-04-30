package controller

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BaselinePolicyName is the name of the operator-managed default-deny baseline.
const BaselinePolicyName = "kube-vnet-default-deny"

// DesiredBaseline returns the desired default-deny baseline NetworkPolicy for a namespace.
//
// The baseline:
//   - Selects all pods in the namespace (empty podSelector).
//   - Sets policyTypes Ingress + Egress so unmatched traffic is dropped.
//   - Allows egress to CoreDNS (UDP/TCP 53) — without this, pods would lose name resolution.
//
// User-managed NetworkPolicies coexist additively (NetworkPolicies are ORed by Kubernetes).
func DesiredBaseline(ns string) *networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	port53 := intstr.FromInt(53)
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
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{NamespaceMetadataNameLabel: KubeSystemNamespace},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{DNSAppLabelKey: DNSAppLabelValue},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &port53},
					{Protocol: &tcp, Port: &port53},
				},
			}},
		},
	}
}
