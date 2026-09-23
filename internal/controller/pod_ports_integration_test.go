//go:build integration

package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// A named targetPort served only by a native sidecar resolves, as the
// EndpointSlice controller resolves it.
func TestIntegration_ExternalAllow_NamedTargetPort_NativeSidecar(t *testing.T) {
	ns := uniqueNS(t, "extallow-sidecar")
	mustCreate(t, makeNamespace(ns, nil, nil))

	svc := makeLBService(ns, "web")
	svc.Spec.Ports = []corev1.ServicePort{{Name: "proxy", Port: 80, TargetPort: intstr.FromString("proxy")}}
	mustCreate(t, svc)

	always := corev1.ContainerRestartPolicyAlways
	mustCreate(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-1", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name: "proxy", Image: "envoy", RestartPolicy: &always,
				Ports: []corev1.ContainerPort{{Name: "proxy", ContainerPort: 15001}},
			}},
			Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
		},
	})

	pol := waitForExternalAllowPolicy(t, ns, "web", 10*time.Second)
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 15001 {
		t.Errorf("port = %d, want 15001 (resolved from the sidecar's 'proxy')", got)
	}
}
