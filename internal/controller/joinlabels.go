package controller

import (
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// userJoinPrefix is the prefix of the user join labels, `kube-vnet/net.*`.
const userJoinPrefix = DefaultLabelPrefix + "net."

// isJoinLabel reports whether k is a user `kube-vnet/net.*` or an operator
// `kube-vnet.system/net.*` label. The generator selects on the latter and the
// diagnostics read the former, so a change in either must enqueue.
func isJoinLabel(k string) bool {
	return strings.HasPrefix(k, userJoinPrefix) || strings.HasPrefix(k, LabelSystemNetPrefix)
}

// HasJoinLabel reports whether obj carries a join label (isJoinLabel).
func HasJoinLabel(obj client.Object) bool {
	return len(joinLabelSet(obj)) > 0
}

// joinLabelSet extracts obj's join labels (isJoinLabel).
func joinLabelSet(obj client.Object) map[string]string {
	out := map[string]string{}
	if obj == nil {
		return out
	}
	for k, v := range obj.GetLabels() {
		if isJoinLabel(k) {
			out[k] = v
		}
	}
	return out
}

func resolvedGeneration(obj client.Object) string {
	if obj == nil {
		return ""
	}
	return obj.GetAnnotations()[AnnotationResolvedGeneration]
}

// JoinLabelChangedPredicate is the change-based pod predicate for the
// VirtualNetworkReconciler. On Update it fires only when the join-label set
// or the resolution marker changed: each reconcile lists every pod, and
// nothing in it depends on pod status, so status churn must not enqueue.
//
// The resolved-generation annotation matters because for a pod whose
// membership is denied, resolution writes only that annotation and no stamp.
// Without it, a vnet reconcile that raced the pod's resolution would leave
// the InvalidJoiners diagnostic stale until the periodic requeue.
//
// Create/Delete/Generic fire for any pod carrying a join label.
func JoinLabelChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return HasJoinLabel(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return HasJoinLabel(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !maps.Equal(joinLabelSet(e.ObjectOld), joinLabelSet(e.ObjectNew)) ||
				resolvedGeneration(e.ObjectOld) != resolvedGeneration(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool { return HasJoinLabel(e.Object) },
	}
}

// isStampedMember reports whether resolution has made pod a member of the vnet
// whose stamp key is stampKey: resolved, and stamped with a direction other
// than none.
func isStampedMember(pod *corev1.Pod, stampKey string) bool {
	if pod.Annotations[AnnotationResolvedGeneration] == "" {
		return false
	}
	dir, ok := ParseBareDirection(pod.Labels[stampKey])
	return ok && dir != DirectionNone
}
