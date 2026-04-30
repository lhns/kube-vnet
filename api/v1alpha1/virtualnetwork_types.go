package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NamespaceSelector matches namespaces by name, by label, or all.
// The home namespace (the namespace the VirtualNetwork lives in) is always
// implicitly included. If multiple fields are set, the union applies — a
// namespace matches if any one of (All, Names, Selector) matches.
type NamespaceSelector struct {
	// All allows pods from any namespace to join. This is the wildcard form.
	// When true, Names and Selector are ignored.
	// +optional
	All bool `json:"all,omitempty"`

	// Names is an explicit list of namespace names allowed to join.
	// Names are matched exactly — no glob/wildcard patterns. Use Selector
	// for label-based grouping; use All for "any namespace".
	// +optional
	Names []string `json:"names,omitempty"`

	// Selector matches namespaces by labels.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// VirtualNetworkSpec is the desired state of a VirtualNetwork.
type VirtualNetworkSpec struct {
	// Description is free text. Not interpreted by the operator.
	// +optional
	Description string `json:"description,omitempty"`

	// AllowedNamespaces controls which namespaces' pods may join this VirtualNetwork.
	// The home namespace is always allowed; if this field is unset, only the home
	// namespace is allowed.
	// +optional
	AllowedNamespaces *NamespaceSelector `json:"allowedNamespaces,omitempty"`
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
	// Known types: Ready, Degraded.
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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="VirtualNetwork name must be a DNS-1123 label (lowercase alphanumeric and hyphens; no dots)"

// VirtualNetwork is a named virtual network. Pods that share a VirtualNetwork
// can communicate; pods on different (or no) VirtualNetworks are isolated by
// the operator-managed NetworkPolicy set. A VirtualNetwork lives in one
// "home" namespace and may permit pods from other namespaces to join via
// spec.allowedNamespaces.
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
