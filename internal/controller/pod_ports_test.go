package controller

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// initContainerWithPort returns an init container declaring port; always
// makes it a native sidecar (restartPolicy Always).
func initContainerWithPort(port corev1.ContainerPort, always bool) corev1.Container {
	c := corev1.Container{Name: "init", Image: "proxy", Ports: []corev1.ContainerPort{port}}
	if always {
		policy := corev1.ContainerRestartPolicyAlways
		c.RestartPolicy = &policy
	}
	return c
}

func TestResolveTargetPorts_NativeSidecar(t *testing.T) {
	sp := corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("http")}
	sel := map[string]string{"app": "web"}
	pod := func(always bool) []corev1.Pod {
		p := podWithHostPorts("p")
		p.Labels = sel
		p.Spec.InitContainers = []corev1.Container{
			initContainerWithPort(corev1.ContainerPort{Name: "http", ContainerPort: 15001}, always),
		}
		return []corev1.Pod{*p}
	}

	got, err := resolveTargetPorts(sp, sel, pod(true))
	if err != nil || len(got) != 1 || got[0] != 15001 {
		t.Errorf("native sidecar: got %v, %v; want [15001]", got, err)
	}

	// An ordinary init container has exited before the pod serves;
	// Kubernetes doesn't resolve named ports against it.
	if got, err := resolveTargetPorts(sp, sel, pod(false)); !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("ordinary init container: got %v, %v; want errNamedPortUnresolvable", got, err)
	}
}

// The kubelet maps hostPorts from spec.containers only, so a hostPort on any
// init container, native sidecar or not, is never forwarded and gets no
// allow.
func TestDesiredHostPortKeys_InitContainersNotCounted(t *testing.T) {
	for _, always := range []bool{true, false} {
		p := podWithHostPorts("p")
		p.Spec.InitContainers = []corev1.Container{
			initContainerWithPort(corev1.ContainerPort{HostPort: 8080, ContainerPort: 80}, always),
		}
		if got := desiredHostPortKeys([]corev1.Pod{*p}); len(got) != 0 {
			t.Errorf("restartPolicy Always=%v: got %v, want no keys", always, got)
		}
	}
}
