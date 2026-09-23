// Package testutil holds test helpers shared by the controller and webhook
// test suites. The two can't share a test package: the webhook package
// imports internal/controller, so a controller test importing the webhook's
// helpers would be an import cycle. This package imports neither.
package testutil

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Eventually polls fn until it returns nil, failing the test with fn's last
// error once timeout passes.
func Eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("eventually: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// UniqueNS returns prefix plus a random suffix, so tests sharing one
// apiserver never collide. (A time-derived suffix did: the clock advances in
// coarse steps, so back-to-back calls returned the same name.)
func UniqueNS(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + rand.String(5)
}

// MustCreate creates obj or fails the test.
func MustCreate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s/%s: %v", obj, obj.GetNamespace(), obj.GetName(), err)
	}
}

// Namespace builds a Namespace carrying the kubernetes.io/metadata.name label
// the apiserver sets, so selector-based tests behave the same against a fake
// client and a real apiserver.
func Namespace(name string, annotations, labels map[string]string) *corev1.Namespace {
	merged := map[string]string{"kubernetes.io/metadata.name": name}
	for k, v := range labels {
		merged[k] = v
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Annotations: annotations, Labels: merged,
	}}
}

// Pod builds a schedulable pod.
func Pod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "registry.k8s.io/pause:3.10"}},
		},
	}
}

// VirtualNetwork builds a vnet; allowed nil means home namespace only.
func VirtualNetwork(name, ns string, allowed *vnetv1alpha1.NamespaceSelector) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       vnetv1alpha1.VirtualNetworkSpec{AllowedNamespaces: allowed},
	}
}

// Scheme returns a scheme with core/v1 and the kube-vnet API, plus extra.
func Scheme(t *testing.T, extra ...func(*runtime.Scheme) error) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range append([]func(*runtime.Scheme) error{corev1.AddToScheme, vnetv1alpha1.AddToScheme}, extra...) {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// Run runs the tests and calls stop on every exit path: normal return, a
// panic in any test, and Ctrl+C / SIGTERM.
func Run(m *testing.M, stop func()) int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stop()
		os.Exit(130)
	}()
	defer stop()
	return m.Run()
}
