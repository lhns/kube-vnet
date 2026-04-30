package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Extent describes the maximum reach of a VirtualNetwork.
// +kubebuilder:validation:Enum=Namespace;Cluster
type Extent string

const (
	ExtentNamespace Extent = "Namespace"
	ExtentCluster   Extent = "Cluster"
)

// VirtualNetworkSpec defines the desired state of a VirtualNetwork.
type VirtualNetworkSpec struct {
	// Extent describes the reach of the VirtualNetwork.
	// Namespace (default): only pods in the same namespace may join.
	// Cluster: pods in any namespace may join, via the namespace-prefixed label form.
	// +kubebuilder:default=Namespace
	// +optional
	Extent Extent `json:"extent,omitempty"`

	// Description is free text surfaced for documentation. The operator does not interpret it.
	// +optional
	Description string `json:"description,omitempty"`
}

// NamespaceMembers groups observed pod members by namespace.
type NamespaceMembers struct {
	Namespace string   `json:"namespace"`
	Pods      []string `json:"pods,omitempty"`
}

// PolicyRef references a generated NetworkPolicy.
type PolicyRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// VirtualNetworkStatus is the observed state of a VirtualNetwork.
type VirtualNetworkStatus struct {
	// Conditions follow the standard Kubernetes condition pattern.
	// Known types: Ready, Degraded, Reconciling.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Members is the observed pod membership grouped by namespace.
	// +optional
	Members []NamespaceMembers `json:"members,omitempty"`

	// GeneratedPolicies lists NetworkPolicy resources owned by this VirtualNetwork.
	// +optional
	GeneratedPolicies []PolicyRef `json:"generatedPolicies,omitempty"`

	// ObservedGeneration is the .metadata.generation last observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vnet;vnets,scope=Namespaced
// +kubebuilder:printcolumn:name="Extent",type=string,JSONPath=`.spec.extent`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualNetwork is a named virtual network. Pods that share a VirtualNetwork can communicate;
// pods on different (or no) VirtualNetworks are isolated by the operator-managed NetworkPolicy set.
type VirtualNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualNetworkSpec   `json:"spec,omitempty"`
	Status VirtualNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualNetworkList contains a list of VirtualNetwork.
type VirtualNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualNetwork{}, &VirtualNetworkList{})
}
