package controller

import (
	"context"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Permits reports whether pods in podNS may join the vnet named by vnetKey,
// so a stamp means the operator confirmed membership, not that a user asked
// for it. A missing vnet or malformed key is (false, nil); errors are
// transient. The bare `cluster` key has no home namespace to fetch and is
// permitted (that vnet allows all namespaces); a qualified `<ns>.cluster`
// names a concrete vnet and is checked like any other (ADR 0043).
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
	return PermitsForVnet(ctx, c, &v, podNS)
}

// NamespacesAdmittedBy is the inverse of Permits: the namespaces that may
// join vnet (`home ∪ allowedNamespaces`). It is a vnet's blast radius: only
// pods there can change membership, whatever names the vnet, so the
// membership sources need no fan-out logic of their own (ADR 0044).
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

// PermitsForVnet is Permits for a caller that already holds the vnet. It
// decides on the object alone, never on its name: the real `cluster` vnet is
// permitted by its `allowedNamespaces: {all: true}`.
func PermitsForVnet(ctx context.Context, c client.Reader, v *vnetv1alpha1.VirtualNetwork, podNS string) (bool, error) {
	if v == nil {
		return false, nil
	}
	if podNS == v.Namespace {
		return true, nil
	}
	return matchesAllowedNamespaces(ctx, c, v.Spec.AllowedNamespaces, podNS)
}

// matchesAllowedNamespaces reports whether sel admits podNS. A nil sel admits
// nothing; the caller has already admitted the home namespace.
func matchesAllowedNamespaces(ctx context.Context, c client.Reader, sel *vnetv1alpha1.NamespaceSelector, podNS string) (bool, error) {
	if sel == nil {
		return false, nil
	}
	if sel.All || slices.Contains(sel.Names, podNS) {
		return true, nil
	}
	if sel.Selector == nil {
		return false, nil
	}
	// A malformed selector is the vnet's own data problem, not a transient
	// error: it matches nothing, like a malformed binding selector.
	s, err := metav1.LabelSelectorAsSelector(sel.Selector)
	if err != nil {
		return false, nil
	}
	var nsObj corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: podNS}, &nsObj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return s.Matches(labels.Set(nsObj.Labels)), nil
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
	homeNS, vnetName, ok = strings.Cut(s, ".")
	if !ok || homeNS == "" || vnetName == "" {
		return "", "", false
	}
	return homeNS, vnetName, true
}
