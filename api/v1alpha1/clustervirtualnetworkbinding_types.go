package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterVirtualNetworkBindingSpec is the desired state of a
// ClusterVirtualNetworkBinding.
//
// A cluster binding attaches the pods selected by spec.podSelector across all
// namespaces matched by spec.namespaceSelector to the referenced VirtualNetwork.
// Useful for operator-wide defaults (e.g. "every pod in every managed
// namespace joins the cluster system-vnet as egress") and for cluster-scoped
// rules that span multiple tenants.
//
// Per-NamespaceVirtualNetworkBinding rules in a matched namespace win over a
// ClusterVirtualNetworkBinding for the same (vnet, pod) pair; pod-authored
// labels win over both. See ADR 0030.
type ClusterVirtualNetworkBindingSpec struct {
	// VirtualNetworkRef is the target VirtualNetwork.
	VirtualNetworkRef VirtualNetworkRef `json:"virtualNetworkRef"`

	// Direction the selected pods participate in.
	// Same enum as the join label value: both | ingress | egress | none.
	// Defaults to "both".
	// +kubebuilder:validation:Enum=both;ingress;egress;none
	// +kubebuilder:default=both
	// +optional
	Direction string `json:"direction,omitempty"`

	// NamespaceSelector picks namespaces this binding applies to. An empty
	// selector matches every managed namespace. Required.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// PodSelector picks pods within the matched namespaces. An empty selector
	// matches every pod. Required.
	PodSelector metav1.LabelSelector `json:"podSelector"`
}

// ClusterVirtualNetworkBindingStatus is the observed state.
type ClusterVirtualNetworkBindingStatus struct {
	// Conditions follow the standard Kubernetes condition pattern.
	// Known types: Ready, Degraded, Conflicts.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// AttachedPods is the observed count of pods (across all matched
	// namespaces) that the operator has honored as members. A count rather
	// than a name list because cluster bindings can match very large pod
	// populations.
	// +optional
	AttachedPods int32 `json:"attachedPods,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvnb;cvnbs,scope=Cluster
// +kubebuilder:printcolumn:name="VNet",type=string,JSONPath=`.spec.virtualNetworkRef.name`
// +kubebuilder:printcolumn:name="VNet-NS",type=string,JSONPath=`.spec.virtualNetworkRef.namespace`
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=`.spec.direction`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.attachedPods`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterVirtualNetworkBinding attaches pods selected by
// spec.namespaceSelector + spec.podSelector to the referenced VirtualNetwork
// across all matching namespaces. Cluster-scoped sibling of
// VirtualNetworkBinding.
//
// Deprecated: ClusterVirtualNetworkBinding is being replaced in two parts
// (ADR 0031). Broad-selector usage migrates to ClusterVirtualNetworkBaseline
// (the dominant case in practice — every cluster-admin-authored CVNB with
// empty selectors). The narrow per-pod cross-namespace case has no direct
// replacement; if needed, write a VirtualNetworkBinding in the target NS.
// Existing CRs continue to function as a backwards-compat shim through 0.4
// (translated to ClusterVirtualNetworkBaseline.memberships entries with
// default-* directions); the kind itself is removed in 0.5.
type ClusterVirtualNetworkBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterVirtualNetworkBindingSpec   `json:"spec,omitempty"`
	Status ClusterVirtualNetworkBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterVirtualNetworkBindingList contains a list of ClusterVirtualNetworkBinding.
type ClusterVirtualNetworkBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterVirtualNetworkBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterVirtualNetworkBinding{}, &ClusterVirtualNetworkBindingList{})
}
