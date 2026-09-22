package controller

import (
	"context"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sweepStalePolicies deletes every NetworkPolicy matching listOpts that is
// not in keep (nil deletes all). Returns on the first delete error.
//
// Reconcilers that identify their policies by `kube-vnet.system/*` labels use
// it to remove anything they no longer want, which also migrates policies
// emitted under an older name format without per-version code. listOpts
// must include the managed-by label: it is the only ownership signal.
func sweepStalePolicies(
	ctx context.Context,
	c client.Client,
	listOpts []client.ListOption,
	keep map[client.ObjectKey]bool,
) error {
	var existing networkingv1.NetworkPolicyList
	if err := c.List(ctx, &existing, listOpts...); err != nil {
		return err
	}
	for i := range existing.Items {
		p := &existing.Items[i]
		key := client.ObjectKey{Namespace: p.Namespace, Name: p.Name}
		if keep[key] {
			continue
		}
		if err := c.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// sweepStalePoliciesByOwner is sweepStalePolicies with ownership decided by
// the controller owner reference (ownerKind, ownerName, ownerUID) instead of
// labels: only matching policies whose controller owner is that object, and
// that are not in keep, are deleted. Owner references survive label-scheme
// changes, so legacy policies are still cleaned up. Use it where every
// policy has a per-resource owner (Service-source policies).
//
// skip, when non-nil, exempts individual policies. Two reconcilers set the
// same Service as owner (ExternalAllow and ApiserverReachable); a sweep that
// cannot narrow its List to one source kind uses skip to leave the other's
// policies alone.
func sweepStalePoliciesByOwner(
	ctx context.Context,
	c client.Client,
	listOpts []client.ListOption,
	ownerKind, ownerName string, ownerUID types.UID,
	keep map[client.ObjectKey]bool,
	skip func(*networkingv1.NetworkPolicy) bool,
) error {
	var existing networkingv1.NetworkPolicyList
	if err := c.List(ctx, &existing, listOpts...); err != nil {
		return err
	}
	for i := range existing.Items {
		p := &existing.Items[i]
		if !hasControllerOwner(p, ownerKind, ownerName, ownerUID) {
			continue
		}
		if skip != nil && skip(p) {
			continue
		}
		key := client.ObjectKey{Namespace: p.Namespace, Name: p.Name}
		if keep[key] {
			continue
		}
		if err := c.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// hasControllerOwner reports whether obj has a controller owner reference
// matching kind, name and uid. Non-controller owner references don't count.
// A policy of a deleted-and-recreated Service fails the UID match; owner-ref
// garbage collection removes it instead.
func hasControllerOwner(obj client.Object, kind, name string, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Kind == kind && ref.Name == name && ref.UID == uid {
			return true
		}
	}
	return false
}

// syncManagedLabels updates `obj`'s labels so the subset matching
// `isManaged` equals `desired`. Three operations in one diff:
//
//   - Add: any key in `desired` that's missing from `obj.Labels`.
//   - Update: any key in `desired` whose value differs.
//   - Remove: any existing label `k` where `isManaged(k)` is true but
//     `k` isn't in `desired`.
//
// The caller does the write; `changed` reports whether one is needed.
func syncManagedLabels(obj client.Object, isManaged func(string) bool, desired map[string]string) (changed bool) {
	labels := obj.GetLabels()
	if labels == nil {
		if len(desired) == 0 {
			return false
		}
		labels = map[string]string{}
		obj.SetLabels(labels)
	}
	// Remove managed labels not in desired.
	for k := range labels {
		if !isManaged(k) {
			continue
		}
		if _, keep := desired[k]; keep {
			continue
		}
		delete(labels, k)
		changed = true
	}
	// Add/update desired labels.
	for k, v := range desired {
		if cur, ok := labels[k]; !ok || cur != v {
			labels[k] = v
			changed = true
		}
	}
	return changed
}

// inNamespacePolicyLabels returns the standard list options for the
// per-NS sweep pattern: scoped to `ns` and filtered by managed-by plus
// any role/source-kind discriminators the caller provides.
func inNamespacePolicyLabels(ns string, extraLabels map[string]string) []client.ListOption {
	merged := map[string]string{LabelManagedBy: LabelManagedByValue}
	for k, v := range extraLabels {
		merged[k] = v
	}
	return []client.ListOption{
		client.InNamespace(ns),
		client.MatchingLabels(merged),
	}
}
