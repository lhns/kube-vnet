package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// The stamps are the operator's output on a pod: the labels resolution
// produces (IsResolutionManagedLabel) plus the two resolved-marker
// annotations. The reconciler and the admission webhook both write them, and
// both go through these helpers so the two modes cannot drift.

// ResolutionStamps returns the resolution-managed labels on pod.
func ResolutionStamps(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	for k, v := range pod.Labels {
		if IsResolutionManagedLabel(k) {
			out[k] = v
		}
	}
	return out
}

// SyncStamps writes desired onto pod in memory and removes stamps resolution
// no longer produces. owns decides which stamp keys the caller may write or
// remove; nil means all. Returns whether any label changed.
//
// The reconciler owns every stamp. The admission webhook owns only the stamps
// the request left untouched: a stamp the request itself added or changed is
// left in place for the validator to judge, so a forged value is rejected
// with a message rather than silently rewritten.
func SyncStamps(pod *corev1.Pod, desired map[string]string, owns func(key string) bool) bool {
	if owns == nil {
		owns = func(string) bool { return true }
	}
	owned := make(map[string]string, len(desired))
	for k, v := range desired {
		if owns(k) {
			owned[k] = v
		}
	}
	return syncManagedLabels(pod, func(k string) bool {
		return IsResolutionManagedLabel(k) && owns(k)
	}, owned)
}

// MarkResolved sets the resolved-marker annotations. resolved-generation is a
// presence marker (membership generation skips pods without it); resolved-by
// records which path stamped the pod.
func MarkResolved(pod *corev1.Pod, by string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationResolvedGeneration] = fmt.Sprintf("%d", pod.Generation)
	pod.Annotations[AnnotationResolvedBy] = by
}

// ClearResolved removes the resolved-marker annotations and reports whether
// either was present.
func ClearResolved(pod *corev1.Pod) bool {
	_, hadGen := pod.Annotations[AnnotationResolvedGeneration]
	_, hadBy := pod.Annotations[AnnotationResolvedBy]
	delete(pod.Annotations, AnnotationResolvedGeneration)
	delete(pod.Annotations, AnnotationResolvedBy)
	return hadGen || hadBy
}

// AnnotationResolvedGeneration marks a pod as resolved, by the reconciler or
// the admission webhook. VirtualNetworkReconciler skips pods without it, so a
// pod is no member until resolution has run (fail closed).
const AnnotationResolvedGeneration = "kube-vnet.system/resolved-generation"

// AnnotationResolvedBy records which path stamped the pod: the admission
// webhook (ADR 0034) or the reconciler. Purely diagnostic — nothing branches
// on it — but when the webhook is unreachable the reconciler takes over
// silently, and this is the only way to see that from the pod itself.
const AnnotationResolvedBy = "kube-vnet.system/resolved-by"

const (
	ResolvedByAdmission  = "admission"
	ResolvedByController = "controller"
)

// LabelSystemNetPrefix is the prefix on operator-stamped membership labels:
// `kube-vnet.system/net.<VnetKey>=<direction>`. Generator selectors match on
// these (not on the user-input `kube-vnet/net.<vnet>` labels). See ADR 0030.
const LabelSystemNetPrefix = "kube-vnet.system/net."
