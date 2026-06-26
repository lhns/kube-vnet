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

// Permits is the single source of truth for "is this pod's NS allowed to
// join this vnet?" — called from three independent code paths that all
// need the same answer:
//
//   - ResolutionReconciler — gates pod-label stamping. If `Permits` says
//     no, the pod doesn't get the `kube-vnet.system/net.*` stamp. This
//     keeps the stamp honest: its presence means the operator confirmed
//     membership, not that a user merely requested it.
//   - VirtualNetworkReconciler.discoverMembers — gates inclusion in the
//     generated membership policy's `from:` rules.
//   - JoinLabelDiagnosticReconciler.diagPrefixed — gates Event emission.
//
// Returns (false, nil) for "not permitted" — covers vnet-doesn't-exist,
// pod NS not in allowedNamespaces, malformed key. Returns (false, err)
// only for transient apiserver/client errors that the caller should retry.
//
// Special case: the cluster vnet is always permitted by design. Its
// allowedNamespaces is `{All: true}` at construction
// (system_vnet_controller.go `ensureClusterSystemVnet`), but Permits
// short-circuits before fetching the CR for the same outcome.
func Permits(ctx context.Context, c client.Reader, vnetKey VnetKey, podNS string) (bool, error) {
	homeNS, vnetName, ok := splitVnetKey(vnetKey)
	if !ok {
		return false, nil
	}
	if vnetName == SystemVnetCluster {
		return true, nil
	}
	if podNS == homeNS {
		return true, nil
	}

	var v vnetv1alpha1.VirtualNetwork
	if err := c.Get(ctx, client.ObjectKey{Namespace: homeNS, Name: vnetName}, &v); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return matchesAllowedNamespaces(ctx, c, v.Spec.AllowedNamespaces, podNS)
}

// PermitsForVnet is a convenience for callers that already have the
// VirtualNetwork object loaded (e.g. VirtualNetworkReconciler's reconcile
// flow that fetched the vnet for other reasons). Saves a redundant Get.
// Same semantics as Permits.
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
