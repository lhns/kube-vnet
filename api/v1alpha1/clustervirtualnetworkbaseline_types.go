package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BaselineMembership is one (vnet, direction) entry inside a baseline's
// memberships list. Directions accept the full eight-value enum (the four
// bare values plus the four `default-*` variants); see ADR 0031.
type BaselineMembership struct {
	// VirtualNetworkRef is the target VirtualNetwork.
	VirtualNetworkRef VirtualNetworkRef `json:"virtualNetworkRef"`

	// Direction the selected pods participate in for this vnet.
	// Bare values (both/ingress/egress/none) are enforced — lower tiers cannot
	// override. The `default-*` variants are advisory — lower tiers may
	// override. See ADR 0031 for the override-permission semantics.
	// +kubebuilder:validation:Enum=both;ingress;egress;none;default-both;default-ingress;default-egress;default-none
	Direction string `json:"direction"`
}

// ClusterVirtualNetworkBaselineSpec is the desired cluster-wide baseline.
//
// One singleton CR named `default` per cluster (enforced by CEL on the type).
// Lists vnet memberships every pod in every managed namespace inherits, with
// per-vnet override-permission encoded in the direction value. Bare values
// cannot be overridden by namespace baselines, bindings, or pod labels;
// default-* values may.
type ClusterVirtualNetworkBaselineSpec struct {
	// Memberships every pod inherits from this cluster baseline. Order is
	// not significant; duplicate vnetRefs surface as a Conflicts condition.
	// +listType=atomic
	Memberships []BaselineMembership `json:"memberships,omitempty"`
}

// ClusterVirtualNetworkBaselineStatus is the observed state.
type ClusterVirtualNetworkBaselineStatus struct {
	// Conditions follow the standard Kubernetes condition pattern.
	// Known types: Ready, Conflicts.
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
// +kubebuilder:resource:shortName=cvnbl;cvnbls,scope=Cluster
// +kubebuilder:printcolumn:name="Memberships",type=integer,JSONPath=`.spec.memberships`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="ClusterVirtualNetworkBaseline must be named 'default' (singleton; ADR 0031)"

// ClusterVirtualNetworkBaseline is the cluster-wide tier-default for vnet
// memberships. Singleton per cluster, named `default`. See ADR 0031.
type ClusterVirtualNetworkBaseline struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterVirtualNetworkBaselineSpec   `json:"spec,omitempty"`
	Status ClusterVirtualNetworkBaselineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterVirtualNetworkBaselineList contains a list of ClusterVirtualNetworkBaseline.
type ClusterVirtualNetworkBaselineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterVirtualNetworkBaseline `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterVirtualNetworkBaseline{}, &ClusterVirtualNetworkBaselineList{})
}
