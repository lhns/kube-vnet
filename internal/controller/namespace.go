package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AnnotationDisabled, when set to "true" on a Namespace, opts that namespace
// out of kube-vnet *entirely*: no baseline policy, no membership policies,
// pods here are not eligible joiners for VirtualNetworks defined elsewhere,
// and any VirtualNetworkBinding here is ignored. The operator-level
// `--disabled-namespaces` flag is the cluster-wide equivalent.
const AnnotationDisabled = "kube-vnet/disabled"

// AnnotationExternalAllow set to "false" on a Service or its Namespace opts
// it out of the allow-from-anywhere policies of both Service-owned
// reconcilers (ADR 0038, 0041). Any other value leaves them on.
const AnnotationExternalAllow = "kube-vnet/external-allow"

// ExternalAllowOptedOut reports whether annotations opt out
// (AnnotationExternalAllow is exactly "false").
func ExternalAllowOptedOut(annotations map[string]string) bool {
	return annotations[AnnotationExternalAllow] == "false"
}

// AnnotationApiserverReachable set to "true" on a Service makes it
// apiserver-reachable although no webhook, APIService or CRD conversion names
// it (ADR 0041): the escape hatch for anything those four kinds don't cover.
const AnnotationApiserverReachable = "kube-vnet/apiserver-reachable"

// ApiserverReachableOptedIn reports whether annotations opt in
// (AnnotationApiserverReachable is exactly "true").
func ApiserverReachableOptedIn(annotations map[string]string) bool {
	return annotations[AnnotationApiserverReachable] == "true"
}

// NamespaceFilter decides whether kube-vnet manages a given namespace.
type NamespaceFilter struct {
	// Excluded is the operator-level exclusion list (from --disabled-namespaces).
	Excluded map[string]bool
}

// NewNamespaceFilter builds a filter excluding the given names; it adds none
// of its own.
func NewNamespaceFilter(excluded []string) *NamespaceFilter {
	set := make(map[string]bool, len(excluded))
	for _, n := range excluded {
		if n != "" {
			set[n] = true
		}
	}
	return &NamespaceFilter{Excluded: set}
}

// IsManaged returns false if the namespace is in the operator-level excluded
// list or carries the AnnotationDisabled annotation set to "true".
func (f *NamespaceFilter) IsManaged(ns *corev1.Namespace) bool {
	return ns != nil && !f.Excluded[ns.Name] && ns.Annotations[AnnotationDisabled] != "true"
}

// Manages reads the namespace name through c and reports IsManaged; a missing
// namespace is not managed.
func (f *NamespaceFilter) Manages(ctx context.Context, c client.Reader, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return f.IsManaged(ns), nil
}
