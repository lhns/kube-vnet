package controller

import (
	corev1 "k8s.io/api/core/v1"
)

// AnnotationDisabled, when set to "true" on a Namespace, opts that namespace
// out of kube-vnet *entirely*: no baseline policy, no membership policies,
// pods here are not eligible joiners for VirtualNetworks defined elsewhere,
// and any VirtualNetworkBinding here is ignored.
//
// This is independent of AnnotationIngressIsolation (which controls only
// the baseline). See ADR 0023.
const AnnotationDisabled = "kube-vnet/disabled"

// AnnotationIngressIsolation, when set on a Namespace to "none" /
// "namespace" / "pod", overrides the operator-level default for that
// namespace's baseline (kube-vnet-default-deny). Independent of
// AnnotationDisabled.
const AnnotationIngressIsolation = "kube-vnet/ingress-isolation"

// NamespaceFilter decides whether kube-vnet manages a given namespace
// AND, if so, what its ingress-isolation mode should be.
type NamespaceFilter struct {
	// Excluded is the operator-level exclusion list (from --excluded-namespaces).
	Excluded map[string]bool

	// DefaultIsolation is the cluster-wide default ingress-isolation mode
	// used when a namespace has no per-namespace annotation and isn't in
	// any of the override lists below. Default zero-value is IsolationNone.
	DefaultIsolation IsolationMode

	// OverrideIsolationNone, OverrideIsolationNamespace, OverrideIsolationPod
	// are the operator-level override lists. A namespace appearing in any
	// of these is forced to that mode (subject to the per-namespace
	// annotation, which overrides further). A namespace appearing in more
	// than one of these is a configuration error and the operator should
	// refuse to start.
	OverrideIsolationNone      map[string]bool
	OverrideIsolationNamespace map[string]bool
	OverrideIsolationPod       map[string]bool
}

// NewNamespaceFilter builds a filter from the given excluded names. The
// operator always seeds defaults (kube-system, kube-public, kube-node-lease)
// and the operator's own namespace at construction time on top of these.
//
// Isolation defaults can be set on the returned struct after construction.
func NewNamespaceFilter(excluded []string) *NamespaceFilter {
	set := make(map[string]bool, len(excluded))
	for _, n := range excluded {
		if n == "" {
			continue
		}
		set[n] = true
	}
	return &NamespaceFilter{
		Excluded:                   set,
		DefaultIsolation:           IsolationNone,
		OverrideIsolationNone:      map[string]bool{},
		OverrideIsolationNamespace: map[string]bool{},
		OverrideIsolationPod:       map[string]bool{},
	}
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

// ResolveIsolation returns the ingress-isolation mode for the given namespace,
// applying the precedence rule:
//
//  1. If the namespace carries kube-vnet/ingress-isolation with a recognized
//     value, that wins.
//  2. Else if the namespace name appears in any of the override lists, the
//     matching mode wins.
//  3. Else DefaultIsolation.
//
// Excluded / disabled namespaces should not be passed here — the caller
// should check IsManaged first.
func (f *NamespaceFilter) ResolveIsolation(ns *corev1.Namespace) IsolationMode {
	if ns == nil {
		return f.DefaultIsolation
	}
	if v, ok := ns.Annotations[AnnotationIngressIsolation]; ok {
		if mode, ok := ParseIsolationMode(v); ok {
			return mode
		}
	}
	if f.OverrideIsolationNone[ns.Name] {
		return IsolationNone
	}
	if f.OverrideIsolationNamespace[ns.Name] {
		return IsolationNamespace
	}
	if f.OverrideIsolationPod[ns.Name] {
		return IsolationPod
	}
	return f.DefaultIsolation
}
