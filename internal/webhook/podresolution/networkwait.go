package podresolution

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// AnnotationNetworkMaxWait opts a pod into the network wait (ADR 0045): its
// app is held until every node has applied its NetworkPolicy rules, for at
// most this Go duration. Absent means no wait.
const AnnotationNetworkMaxWait = "kube-vnet/network-max-wait"

// NetworkWaitContainerName is the init container injected into opted-in pods.
const NetworkWaitContainerName = "kube-vnet-network-wait"

// NetworkWaitConfig enables the network wait. The chart sets both from its
// own values; the webhook can't otherwise know them.
type NetworkWaitConfig struct {
	Image   string // the operator image; the init container runs its network-wait subcommand
	Beacons string // the beacons' headless Service, host:port
}

// networkWait decides whether pod gets the wait container. A pod that asked
// for it but can't have it gets a warning instead, so the opt-in never fails
// silently; it is never rejected, since the wait is a convenience.
func networkWait(pod *corev1.Pod, cfg *NetworkWaitConfig) (*corev1.Container, string) {
	value, asked := pod.Annotations[AnnotationNetworkMaxWait]
	if !asked || pod.Spec.HostNetwork {
		return nil, "" // hostNetwork pods aren't subject to NetworkPolicy
	}
	if cfg == nil {
		return nil, fmt.Sprintf("%s is set, but the network wait is not enabled on this cluster "+
			"(chart value webhook.networkWait.enabled); the pod starts without waiting", AnnotationNetworkMaxWait)
	}
	maxWait, err := time.ParseDuration(value)
	if err != nil || maxWait <= 0 {
		return nil, fmt.Sprintf("%s=%q is not a positive duration such as \"30s\"; "+
			"the pod starts without waiting", AnnotationNetworkMaxWait, value)
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == NetworkWaitContainerName {
			return nil, "" // already injected (webhook reinvocation)
		}
	}

	no, yes := false, true
	return &corev1.Container{
		Name:            NetworkWaitContainerName,
		Image:           cfg.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"network-wait", "--beacons=" + cfg.Beacons, "--max-wait=" + maxWait.String()},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             &yes,
			AllowPrivilegeEscalation: &no,
			ReadOnlyRootFilesystem:   &yes,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}, ""
}

// unmanagedNetworkWait is the warning for a pod that asked for the wait in a
// namespace kube-vnet doesn't manage, where it gets no wait (nor any
// NetworkPolicy from kube-vnet).
func unmanagedNetworkWait(pod *corev1.Pod) string {
	if _, asked := pod.Annotations[AnnotationNetworkMaxWait]; !asked || pod.Spec.HostNetwork {
		return ""
	}
	return fmt.Sprintf("%s is set, but kube-vnet does not manage this namespace; "+
		"the pod starts without waiting", AnnotationNetworkMaxWait)
}
