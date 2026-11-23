package controller

import (
	"iter"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// resolveTargetPorts returns the sorted pod-side ports a Service port reaches.
// An unset targetPort defaults to the Service port. A named targetPort
// resolves per backing pod, as the endpoints controller does, so pods that
// map the name to different numbers (mid-rollout) each get theirs; with no
// backing pod declaring the name it returns errNamedPortUnresolvable.
func resolveTargetPorts(sp corev1.ServicePort, selector map[string]string, pods []corev1.Pod) ([]int32, error) {
	tp := sp.TargetPort
	if tp.Type == intstr.Int && tp.IntVal != 0 {
		return []int32{tp.IntVal}, nil
	}
	if tp.Type != intstr.String || tp.StrVal == "" {
		return []int32{sp.Port}, nil
	}
	var out []int32
	for _, p := range pods {
		if !labelsMatchSelector(p.Labels, selector) {
			continue
		}
		for c := range namedPortContainers(&p.Spec) {
			for _, cp := range c.Ports {
				if cp.Name == tp.StrVal {
					out = append(out, cp.ContainerPort)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, errNamedPortUnresolvable
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// labelsMatchSelector returns true if `labels` contains every key/value pair
// in `selector`. An empty selector matches nothing.
func labelsMatchSelector(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// hasNamedTargetPort returns true if any Service port uses a string
// (named) targetPort that needs Pod-side resolution.
func hasNamedTargetPort(svc *corev1.Service) bool {
	for _, p := range svc.Spec.Ports {
		if p.TargetPort.Type == intstr.String && p.TargetPort.StrVal != "" {
			return true
		}
	}
	return false
}
