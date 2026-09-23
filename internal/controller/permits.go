package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Permits reports whether pods in podNS may join the vnet named by vnetKey.
// Resolution uses it to decide which vnets to stamp on a pod, so a stamp
// means the operator confirmed membership rather than that a user merely
// asked for it; PermitsForVnet applies the same rule when generating policies.
//
// Returns (false, nil) when not permitted, including a missing vnet or a
// malformed key, and an error only for transient failures the caller should
// retry.
//
// The bare `cluster` key (ADR 0033 Amendment) has no home namespace to fetch
// and is permitted directly; the cluster vnet allows all namespaces anyway.
// A qualified `<ns>.cluster` key is not short-circuited: it names a concrete
// vnet, and a wrong namespace must be denied like any missing vnet (ADR 0043).
func Permits(ctx context.Context, c client.Reader, vnetKey VnetKey, podNS string) (bool, error) {
	homeNS, vnetName, ok := splitVnetKey(vnetKey)
	if !ok {
		return false, nil
	}
	if vnetName == SystemVnetCluster && homeNS == "" {
		return true, nil
	}

	// Check existence before the home-namespace short-circuit, or a bare
	// label naming a missing local vnet would be stamped.
	var v vnetv1alpha1.VirtualNetwork
	if err := c.Get(ctx, client.ObjectKey{Namespace: homeNS, Name: vnetName}, &v); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if podNS == v.Namespace {
		return true, nil
	}
	return matchesAllowedNamespaces(ctx, c, v.Spec.AllowedNamespaces, podNS)
}

// NamespacesAdmittedBy is the inverse of Permits: given a vnet, which
// namespaces may join it (`home ∪ allowedNamespaces`)?
//
// This is the blast radius of a VirtualNetwork. A pod's membership can only
// change if its namespace may join, so a vnet event needs to reach exactly the
// pods in these namespaces — whatever named the vnet (join label, binding, or
// either baseline). Expressing fan-out this way means the four membership
// sources don't each re-derive permission logic. See ADR 0044.
//
// It delegates to PermitsForVnet so `home ∪ allowedNamespaces` keeps one
// definition; namespaces are few and the client is cached.
func NamespacesAdmittedBy(ctx context.Context, c client.Reader, vnet *vnetv1alpha1.VirtualNetwork) ([]string, error) {
	if vnet == nil {
		return nil, nil
	}
	var namespaces corev1.NamespaceList
	if err := c.List(ctx, &namespaces); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(namespaces.Items))
	for i := range namespaces.Items {
		ns := namespaces.Items[i].Name
		ok, err := PermitsForVnet(ctx, c, vnet, ns)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ns)
		}
	}
	return out, nil
}

// PermitsForVnet is Permits for a caller that already holds the vnet.
func PermitsForVnet(ctx context.Context, c client.Reader, v *vnetv1alpha1.VirtualNetwork, podNS string) (bool, error) {
	if v == nil {
		return false, nil
	}
	if v.Name == SystemVnetCluster {
		return true, nil
	}
	if podNS == v.Namespace {
		return true, nil
	}
	return matchesAllowedNamespaces(ctx, c, v.Spec.AllowedNamespaces, podNS)
}

// matchesAllowedNamespaces implements the per-vnet NamespaceSelector
// check shared by both Permits entry points. nil selector means
// "home NS only" (caller already returned true if podNS == home).
func matchesAllowedNamespaces(ctx context.Context, c client.Reader, sel *vnetv1alpha1.NamespaceSelector, podNS string) (bool, error) {
	if sel == nil {
		return false, nil
	}
	if sel.All {
		return true, nil
	}
	for _, n := range sel.Names {
		if n == podNS {
			return true, nil
		}
	}
	if sel.Selector != nil {
		var nsObj corev1.Namespace
		if err := c.Get(ctx, client.ObjectKey{Name: podNS}, &nsObj); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		s, err := metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return false, err
		}
		if s.Matches(labels.Set(nsObj.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

// splitVnetKey decomposes a canonical VnetKey into (homeNS, vnetName).
// VnetKey format is `<homeNS>.<vnetName>` per ADR 0033, with the cluster
// singleton bare `cluster` per the ADR 0033 Amendment. For cluster,
// homeNS is the empty string — callers using homeNS for an apiserver
// Get should special-case cluster before splitting.
func splitVnetKey(k VnetKey) (homeNS, vnetName string, ok bool) {
	s := string(k)
	if s == "" {
		return "", "", false
	}
	if s == SystemVnetCluster {
		return "", SystemVnetCluster, true
	}
	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
