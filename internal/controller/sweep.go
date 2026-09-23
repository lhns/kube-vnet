package controller

import (
	"context"
	"maps"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// applyPolicy server-side-applies desired with the operator's field manager,
// unless the live policy read through reader already matches it on
// everything the apply sets (policyUpToDate). Every policy-writing reconciler
// applies through here, so a reconcile that changes nothing writes nothing.
// Reading the live object keeps drift correction: a policy someone edited no
// longer matches and is re-applied. created reports that the policy was
// absent before the apply.
//
// reader is normally the cached client: a cache that lags a manual edit or
// delete still converges, since that change's own watch event re-enqueues.
func applyPolicy(ctx context.Context, c client.Client, reader client.Reader, desired *networkingv1.NetworkPolicy) (created bool, err error) {
	live := &networkingv1.NetworkPolicy{}
	switch err := reader.Get(ctx, client.ObjectKeyFromObject(desired), live); {
	case apierrors.IsNotFound(err):
		created = true
	case err != nil:
		return false, err
	case policyUpToDate(live, desired):
		return false, nil
	}
	desired.SetResourceVersion("")
	return created, c.Patch(ctx, desired, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership)
}

// policyUpToDate reports whether applying desired over live would change
// nothing the operator manages: the spec, the owner references and the labels
// must be equal, and every desired annotation present. The labels compare
// exactly, so a label a previous version applied and this one dropped is
// still removed. Annotations don't compare exactly, so one a builder stopped
// setting would linger until some other change forces an apply; no builder
// sets any today. Anything else on live (another manager's annotations,
// status, managedFields) is not the apply's to change. The spec compares
// semantically (nil and empty are equal); the builders set the fields the
// apiserver would default (port protocol, policyTypes), so a policy read back
// equals its desired form.
func policyUpToDate(live, desired *networkingv1.NetworkPolicy) bool {
	if live.DeletionTimestamp != nil || !maps.Equal(live.Labels, desired.Labels) {
		return false
	}
	for k, v := range desired.Annotations {
		if cur, ok := live.Annotations[k]; !ok || cur != v {
			return false
		}
	}
	return equality.Semantic.DeepEqual(live.OwnerReferences, desired.OwnerReferences) &&
		equality.Semantic.DeepEqual(live.Spec, desired.Spec)
}

// sweepStalePolicies deletes every NetworkPolicy matching listOpts that is
// not in keep (nil deletes all) and not exempted by skip (nil exempts none).
// Returns on the first delete error.
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
	skip func(*networkingv1.NetworkPolicy) bool,
) error {
	var existing networkingv1.NetworkPolicyList
	if err := c.List(ctx, &existing, listOpts...); err != nil {
		return err
	}
	for i := range existing.Items {
		p := &existing.Items[i]
		key := client.ObjectKey{Namespace: p.Namespace, Name: p.Name}
		if keep[key] || (skip != nil && skip(p)) {
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

// syncManagedLabels makes obj's labels for which isManaged is true equal
// desired, leaving the others alone. The caller does the write; changed
// reports whether one is needed.
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

// inNamespacePolicyLabels returns list options selecting the managed
// policies in ns that also carry extraLabels.
func inNamespacePolicyLabels(ns string, extraLabels map[string]string) []client.ListOption {
	merged := map[string]string{LabelManagedBy: LabelManagedByValue}
	maps.Copy(merged, extraLabels)
	return []client.ListOption{
		client.InNamespace(ns),
		client.MatchingLabels(merged),
	}
}
