package controller

import (
	corev1 "k8s.io/api/core/v1"
)

// AnnotationDisabled, when set to "true" on a Namespace, opts that namespace
// out of kube-vnet *entirely*: no baseline policy, no membership policies,
// pods here are not eligible joiners for VirtualNetworks defined elsewhere,
// and any VirtualNetworkBinding here is ignored. The operator-level
// `--disabled-namespaces` flag is the cluster-wide equivalent.
const AnnotationDisabled = "kube-vnet/disabled"

// NamespaceFilter decides whether kube-vnet manages a given namespace.
// (The "what shape is the baseline" question that earlier versions answered
// here is gone — under ADR 0030 the baseline is unconditionally deny-all
// minus the operator's `--elide-baseline-for` exemptions.)
type NamespaceFilter struct {
	// Excluded is the operator-level exclusion list (from --disabled-namespaces).
	Excluded map[string]bool
}

// NewNamespaceFilter builds a filter from the given excluded names. The
// caller is responsible for seeding system namespaces and the operator's
// own namespace; this constructor doesn't add anything implicit.
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

// IsManagedName returns false if the namespace name is in the operator-level
// exclusion list. Use IsManaged when you have the Namespace object (it
// additionally honors the per-namespace annotation).
func (f *NamespaceFilter) IsManagedName(name string) bool {
	return !f.Excluded[name]
}

// IsManaged returns false if the namespace is in the operator-level excluded
// list or carries the AnnotationDisabled annotation set to "true".
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
