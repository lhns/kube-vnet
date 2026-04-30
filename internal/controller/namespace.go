package controller

import (
	corev1 "k8s.io/api/core/v1"
)

// AnnotationDisabled, when set to "true" on a Namespace, opts that namespace out
// of kube-vnet entirely: no baseline policy is created, no membership policies
// are generated for pods in this namespace, and pods here are not eligible
// joiners for VirtualNetworks defined elsewhere (regardless of allowedNamespaces).
const AnnotationDisabled = "kube-vnet/disabled"

// NamespaceFilter decides whether kube-vnet manages a given namespace.
type NamespaceFilter struct {
	// Excluded is the operator-level exclusion list (from --excluded-namespaces).
	Excluded map[string]bool
}

// NewNamespaceFilter builds a filter from the given excluded names. The operator
// always seeds defaults (kube-system, kube-public, kube-node-lease) and the operator's
// own namespace at construction time on top of these.
func NewNamespaceFilter(excluded []string) *NamespaceFilter {
	set := make(map[string]bool, len(excluded))
	for _, n := range excluded {
		if n == "" {
			continue
		}
		set[n] = true
	}
	return &NamespaceFilter{Excluded: set}
}

// IsManagedName returns false if the namespace name is in the operator-level exclusion list.
// Use IsManaged when you have the Namespace object (it additionally honors the per-namespace annotation).
func (f *NamespaceFilter) IsManagedName(name string) bool {
	return !f.Excluded[name]
}

// IsManaged returns false if the namespace is in the operator-level excluded list
// or carries the AnnotationDisabled annotation set to "true".
func (f *NamespaceFilter) IsManaged(ns *corev1.Namespace) bool {
	if ns == nil {
		return false
	}
	if f.Excluded[ns.Name] {
		return false
	}
	if v, ok := ns.Annotations[AnnotationDisabled]; ok && v == "true" {
		return false
	}
	return true
}
