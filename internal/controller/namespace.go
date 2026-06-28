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

// AnnotationExternalAllow, when set to "false" on a Service or a Namespace,
// opts that target out of the ExternalAllowReconciler's auto-emit of
// allow-from-anywhere NetworkPolicies for externally-exposed Services
// (ADR 0038). Any value other than "false" (absent, empty, "true", anything
// else) leaves auto-emit on — the annotation is one-way for explicit opt-out.
const AnnotationExternalAllow = "kube-vnet/external-allow"

// ExternalAllowOptedOut returns true if the resource's annotations explicitly
// disable external-allow auto-emission. Only the literal "false" value opts
// out — every other value leaves auto-emit on.
func ExternalAllowOptedOut(annotations map[string]string) bool {
	return annotations[AnnotationExternalAllow] == "false"
}

// AnnotationApiserverReachable, when set to "true" on a Service, opts that
// Service into the ApiserverReachableReconciler's auto-emit even when no
// admission-webhook / APIService / CRD-conversion discovery resource
// references it (ADR 0041). The four built-in discovery kinds cover ~95%
// of real-world use; this annotation is the escape hatch for future K8s
// APIs, third-party operators with custom webhook-shaped CRDs, or any
// Service the admin manually wants apiserver-reachable.
//
// Symmetric with the opt-OUT annotation AnnotationExternalAllow — both
// reconcilers honor `kube-vnet/external-allow=false` to disable auto-emit
// regardless of how the Service was discovered.
const AnnotationApiserverReachable = "kube-vnet/apiserver-reachable"

// ApiserverReachableOptedIn returns true if the Service explicitly opts
// into apiserver-reachable auto-emission via annotation. Used only as a
// supplementary discovery path; the four built-in discovery resource
// kinds are the primary trigger.
func ApiserverReachableOptedIn(annotations map[string]string) bool {
	return annotations[AnnotationApiserverReachable] == "true"
}

// NamespaceFilter decides whether kube-vnet manages a given namespace.
// (The "what shape is the baseline" question that earlier versions answered
// here is gone — under ADR 0030 + ADR 0035 the baseline is unconditionally
// deny-all selecting every pod; there's no shape knob.)
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
