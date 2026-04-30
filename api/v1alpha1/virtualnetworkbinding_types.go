package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualNetworkRef references a VirtualNetwork by name and (home) namespace.
type VirtualNetworkRef struct {
	// Name of the target VirtualNetwork.
	Name string `json:"name"`
	// Namespace is the home namespace of the target VirtualNetwork.
	Namespace string `json:"namespace"`
}

// VirtualNetworkBindingSpec is the desired state of a VirtualNetworkBinding.
//
// A binding attaches the pods selected by spec.podSelector (in this binding's
// own namespace) to the referenced VirtualNetwork. Useful when the pod
// template can't be modified (third-party manifests, Helm charts, other
// operators). Direct join labels remain the primary, simpler mechanism;
// bindings are the escape hatch.
type VirtualNetworkBindingSpec struct {
	// VirtualNetworkRef is the target VirtualNetwork.
	VirtualNetworkRef VirtualNetworkRef `json:"virtualNetworkRef"`

	// Direction the selected pods participate in.
	// Same enum as the join label value: both | ingress | egress | none.
	// Defaults to "both".
	// +kubebuilder:validation:Enum=both;ingress;egress;none
	// +kubebuilder:default=both
	// +optional
	Direction string `json:"direction,omitempty"`

	// PodSelector selects pods *in this binding's own namespace*. Required.
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
