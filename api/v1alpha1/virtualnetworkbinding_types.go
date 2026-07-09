package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualNetworkRef references a VirtualNetwork by name and (home) namespace.
type VirtualNetworkRef struct {
	// Name of the target VirtualNetwork.
	Name string `json:"name"`

	// Namespace is the home namespace of the target VirtualNetwork.
	//
	// Optional, and omitting it is the recommended form for the reserved
	// system vnets. When omitted it is inferred:
	//
	//   cluster  -> the cluster-wide singleton (lives in the operator's namespace)
	//   namespace -> the referencing pod's own namespace
	//   <user vnet> -> the referencing pod's own namespace
	//
	// When set, it is honored verbatim — never rewritten. A namespace that
	// does not hold the named VirtualNetwork, or one whose vnet does not
	// permit the pod's namespace via spec.allowedNamespaces, simply means
	// the pod cannot join: the membership is dropped and a
	// VirtualNetworkNotJoinable Warning Event is emitted. This applies
	// uniformly to system and user vnets alike.
	//
	// Note the per-namespace `namespace` system vnet exists in every managed
	// namespace — NOT in the operator's release namespace, which is
	// unmanaged. Pointing at the release namespace names a vnet that does
	// not exist. See ADR 0043.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VirtualNetworkBindingSpec is the desired state of a VirtualNetworkBinding.
//
// A binding attaches the pods selected by spec.podSelector (in this binding's
// own namespace) to the referenced VirtualNetwork. Per ADR 0031 the
// podSelector must select at least one pod (no empty matchLabels +
// matchExpressions) — namespace-wide defaults belong in a
// VirtualNetworkBaseline. Direction is restricted to bare values; default-*
// is meaningful only at baseline tiers.
//
// +kubebuilder:validation:XValidation:rule="(has(self.podSelector.matchLabels) && size(self.podSelector.matchLabels) > 0) || (has(self.podSelector.matchExpressions) && size(self.podSelector.matchExpressions) > 0)",message="podSelector must select at least one pod (use VirtualNetworkBaseline for namespace-wide defaults; ADR 0031)"
type VirtualNetworkBindingSpec struct {
	// VirtualNetworkRef is the target VirtualNetwork.
	VirtualNetworkRef VirtualNetworkRef `json:"virtualNetworkRef"`

	// Direction the selected pods participate in.
	// Same enum as the join label value: both | ingress | egress | none.
	// Defaults to "both". The default-* baseline-tier variants are not
	// permitted here (ADR 0031).
	// +kubebuilder:validation:Enum=both;ingress;egress;none
	// +kubebuilder:default=both
	// +optional
	Direction string `json:"direction,omitempty"`

	// PodSelector selects pods *in this binding's own namespace*. Required;
	// must be non-empty (ADR 0031).
	PodSelector metav1.LabelSelector `json:"podSelector"`
}

// VirtualNetworkBindingStatus is the observed state.
type VirtualNetworkBindingStatus struct {
	// Conditions follow the standard Kubernetes condition pattern.
	// Known types: Ready, Degraded.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// AttachedPods is the observed list of pods currently selected by
	// spec.podSelector that the operator has honored as members.
	// +optional
	AttachedPods []string `json:"attachedPods,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vnb;vnbs,scope=Namespaced
// +kubebuilder:printcolumn:name="VNet",type=string,JSONPath=`.spec.virtualNetworkRef.name`
// +kubebuilder:printcolumn:name="VNet-NS",type=string,JSONPath=`.spec.virtualNetworkRef.namespace`
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=`.spec.direction`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualNetworkBinding attaches pods selected by spec.podSelector to the
// referenced VirtualNetwork without requiring those pods to carry the join
// label. The binding lives in the same namespace as the pods it selects.
type VirtualNetworkBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualNetworkBindingSpec   `json:"spec,omitempty"`
	Status VirtualNetworkBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualNetworkBindingList contains a list of VirtualNetworkBinding.
type VirtualNetworkBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualNetworkBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualNetworkBinding{}, &VirtualNetworkBindingList{})
}
