package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualNetworkBaselineSpec is the namespace-wide tier-default.
//
// One singleton CR named `default` per namespace. Inherits from
// ClusterVirtualNetworkBaseline (overriding only entries the cluster
// baseline marked as default-*). See ADR 0031.
type VirtualNetworkBaselineSpec struct {
	// Memberships every pod in this namespace inherits.
	// +listType=atomic
	Memberships []BaselineMembership `json:"memberships,omitempty"`
}

// VirtualNetworkBaselineStatus is the observed state.
type VirtualNetworkBaselineStatus struct {
	// Conditions follow the standard Kubernetes condition pattern.
	// Known types: Ready, Conflicts, OverrideRejected.
	//
	// OverrideRejected fires when this baseline tries to override a vnet
	// that the cluster baseline pinned with a bare (non-default-*) direction;
	// the cluster value remains in effect and this baseline's entry is
	// ignored for that vnet. The condition message names the affected vnet.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vnbl;vnbls,scope=Namespaced
// +kubebuilder:printcolumn:name="Memberships",type=integer,JSONPath=`.spec.memberships`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="VirtualNetworkBaseline must be named 'default' (singleton per namespace; ADR 0031)"

// VirtualNetworkBaseline is the namespace-wide tier-default for vnet
// memberships. Singleton per namespace, named `default`. See ADR 0031.
type VirtualNetworkBaseline struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualNetworkBaselineSpec   `json:"spec,omitempty"`
	Status VirtualNetworkBaselineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualNetworkBaselineList contains a list of VirtualNetworkBaseline.
type VirtualNetworkBaselineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualNetworkBaseline `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualNetworkBaseline{}, &VirtualNetworkBaselineList{})
}
