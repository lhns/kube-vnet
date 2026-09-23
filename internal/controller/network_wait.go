package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// AnnotationNetworkMaxWait opts a pod into the network wait (ADR 0045): its
// app is held until every node has applied its NetworkPolicy rules, for at
// most this Go duration. Absent means no wait.
const AnnotationNetworkMaxWait = "kube-vnet/network-max-wait"

// NetworkWaitContainerName is the init container injected into opted-in pods.
const NetworkWaitContainerName = "kube-vnet-network-wait"

// ReasonNetworkWaitSkipped is the pod Event for a pod that asked for the
// network wait but starts without it. The admission webhook returns the same
// text as a warning, but that reaches only the pod's direct creator, which
// for a Deployment or Job pod is a controller.
const ReasonNetworkWaitSkipped = "NetworkWaitSkipped"

// NetworkWaitWarning returns why pod, which asks for the network wait, will
// not get it, or "" if it gets it or doesn't ask. managed is whether kube-vnet
// manages the pod's namespace; enabled whether the wait is configured.
// hostNetwork pods are never subject to NetworkPolicy, so never warned.
func NetworkWaitWarning(pod *corev1.Pod, managed, enabled bool) string {
	value, asked := pod.Annotations[AnnotationNetworkMaxWait]
	switch {
	case !asked || pod.Spec.HostNetwork:
		return ""
	case !managed:
		return fmt.Sprintf("%s is set, but kube-vnet does not manage this namespace; "+
			"the pod starts without waiting", AnnotationNetworkMaxWait)
	case !enabled:
		return fmt.Sprintf("%s is set, but the network wait is not enabled on this cluster "+
			"(chart value webhook.networkWait.enabled); the pod starts without waiting", AnnotationNetworkMaxWait)
	}
	if d, err := time.ParseDuration(value); err != nil || d <= 0 {
		return fmt.Sprintf("%s=%q is not a positive duration such as \"30s\"; "+
			"the pod starts without waiting", AnnotationNetworkMaxWait, value)
	}
	return ""
}

// networkWaitMissing reports the one case only a look at the created pod
// shows: it asked for the wait, the wait was possible, and yet the pod has
// no wait container, because the webhook was unreachable at creation
// (failurePolicy Ignore) or the annotation was added afterwards.
func networkWaitMissing(pod *corev1.Pod) string {
	if _, asked := pod.Annotations[AnnotationNetworkMaxWait]; !asked || pod.Spec.HostNetwork {
		return ""
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == NetworkWaitContainerName {
			return ""
		}
	}
	return fmt.Sprintf("%s is set, but the pod has no %s init container: the admission webhook did not "+
		"inject it at creation (it was unreachable, or the annotation was added later); the pod started "+
		"without waiting", AnnotationNetworkMaxWait, NetworkWaitContainerName)
}
