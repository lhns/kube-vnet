package controller

import (
	"context"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sweepStalePolicies deletes every operator-managed NetworkPolicy matching
// the given label set that ISN'T in the `keep` set (keyed by
// namespace/name). Returns on the first delete error.
//
// This is the canonical self-healing primitive — every reconciler that
// owns a set of policies identified by a `kube-vnet.system/*` label
// uses it to keep state consistent without per-version migration code:
//
//   - Stale objects (different name than desired) get cleaned up.
//   - Drift correction is automatic: an object with the right labels but
//     a name we don't currently want is deleted on the next reconcile.
//   - Rename migrations are automatic too: a policy emitted under an
//     older name format but still carrying the same role/source-kind
//     labels gets swept the same way.
//
// The shared filter on `kube-vnet.system/managed-by=kube-vnet` (plus any
// reconciler-specific labels passed by the caller) means a policy that
// LOST its managed-by label silently becomes an orphan — the operator
// stops touching it. SSA's ForceOwnership repaints those labels on next
// reconcile of the desired object, so the orphan window is one reconcile
// cycle wide.
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

// sweepStalePoliciesByOwner is a variant of sweepStalePolicies that uses
// the controller-owner reference — not labels — as the per-resource
// ownership check. For each policy matching `listOpts`:
//
//   - If its controller-owner ref points at (ownerKind, ownerName, ownerUID)
//     AND its namespace/name isn't in `keep`, the policy is deleted.
//   - Otherwise (different owner, no owner, kept name), it's left alone.
//
// Owner-ref-based identification survives label-format changes across
// operator versions: a legacy policy emitted before a labeling scheme
// changed still carries its OwnerReference and gets cleaned up here on
// the next reconcile of its owner. Use this for resources where each
// emitted policy has a stable per-resource owner (e.g. Service-source
// external-allow policies); use sweepStalePolicies for resources keyed
// purely by labels (host-source policies have no per-pod owner, NS
// baselines have no owner, etc.).
//
// `keep` of nil or empty means "delete every policy owned by this owner
// matching the filter."
func sweepStalePoliciesByOwner(
	ctx context.Context,
	c client.Client,
	listOpts []client.ListOption,
	ownerKind, ownerName string, ownerUID types.UID,
	keep map[client.ObjectKey]bool,
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

// hasControllerOwner returns true if `obj` carries a controller-flagged
// OwnerReference matching all three of kind, name, and uid. The
// Controller field must be a true pointer (not nil, not false) —
// non-controller owner refs (additional owners) don't count, so we
// don't accidentally claim policies for which we're a secondary owner.
//
// The strict UID match means a Service that was deleted and recreated
// with the same name produces a UID-mismatch on its legacy policy. The
// legacy policy is then handled by apiserver GC's owner-ref cascade
// when the old Service was deleted, so it's gone by the time the new
// Service's reconcile runs — two independent cleanup paths that
// converge on the same outcome.
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
// Stateless — caller controls the patch (SSA, MergeFrom, plain Update).
// Returns `changed == true` if any of `obj`'s labels were modified, so
// the caller can skip the API write on no-op reconciles.
//
// Use cases:
//   - ResolutionReconciler: pod's `kube-vnet.system/net.*` membership
//     stamps + `kube-vnet.system/host-port.*` exposure stamps.
//   - Any future code that stamps a managed-label set on an object and
//     wants old labels removed when the set shrinks.
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
