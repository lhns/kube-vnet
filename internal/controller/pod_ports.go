package controller

import (
	"iter"

	corev1 "k8s.io/api/core/v1"
)

// namedPortContainers yields the containers a Service's named targetPort
// resolves against, in the order Kubernetes searches them: the app
// containers, then native sidecars (init containers with restartPolicy
// Always). Ordinary init containers have exited by the time the pod serves,
// so they don't count. This mirrors FindPort in k8s.io/endpointslice.
func namedPortContainers(spec *corev1.PodSpec) iter.Seq[*corev1.Container] {
	return func(yield func(*corev1.Container) bool) {
		for i := range spec.Containers {
			if !yield(&spec.Containers[i]) {
				return
			}
		}
		for i := range spec.InitContainers {
			c := &spec.InitContainers[i]
			if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
				continue
			}
			if !yield(c) {
				return
			}
		}
	}
}
