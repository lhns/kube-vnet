package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSyncManagedLabels_AddsAndRemoves(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"kube-vnet.system/net.old":       "both",     // managed, will be removed
			"kube-vnet.system/net.payments":  "ingress",  // managed, will be updated
			"kube-vnet.system/host-port.stale.tcp": "true", // managed (different prefix), will be removed
			"app":                            "demo",     // unmanaged, untouched
		}},
	}
	isManaged := func(k string) bool {
		return strings.HasPrefix(k, "kube-vnet.system/net.") ||
			strings.HasPrefix(k, "kube-vnet.system/host-port.")
	}
	desired := map[string]string{
		"kube-vnet.system/net.payments":     "both",   // update
		"kube-vnet.system/net.new":          "egress", // add
		"kube-vnet.system/host-port.80.tcp": "true",   // add (different prefix family)
	}
	changed := syncManagedLabels(pod, isManaged, desired)
	if !changed {
		t.Error("expected changed=true")
	}
	want := map[string]string{
		"app":                               "demo",
		"kube-vnet.system/net.payments":     "both",
		"kube-vnet.system/net.new":          "egress",
		"kube-vnet.system/host-port.80.tcp": "true",
	}
	for k, v := range want {
		if pod.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if _, ok := pod.Labels["kube-vnet.system/net.old"]; ok {
		t.Error("kube-vnet.system/net.old should have been removed")
	}
	if _, ok := pod.Labels["kube-vnet.system/host-port.stale.tcp"]; ok {
		t.Error("stale host-port label should have been removed")
	}
	if len(pod.Labels) != len(want) {
		t.Errorf("label set size = %d, want %d (%v)", len(pod.Labels), len(want), pod.Labels)
	}
}

func TestSyncManagedLabels_NoOp(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"kube-vnet.system/net.x": "both",
		"unmanaged":              "y",
	}}}
	isManaged := func(k string) bool { return strings.HasPrefix(k, "kube-vnet.system/") }
	desired := map[string]string{"kube-vnet.system/net.x": "both"}
	if changed := syncManagedLabels(pod, isManaged, desired); changed {
		t.Error("expected changed=false for no-op call")
	}
}

func TestSyncManagedLabels_RemoveAll(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"kube-vnet.system/net.a": "both",
		"kube-vnet.system/net.b": "ingress",
		"app":                    "demo",
	}}}
	isManaged := func(k string) bool { return strings.HasPrefix(k, "kube-vnet.system/") }
	// Empty desired → remove all managed labels.
	if changed := syncManagedLabels(pod, isManaged, nil); !changed {
		t.Error("expected changed=true")
	}
	if len(pod.Labels) != 1 || pod.Labels["app"] != "demo" {
		t.Errorf("expected only app=demo to remain, got %v", pod.Labels)
	}
}

func TestSyncManagedLabels_NilLabelsNoDesired(t *testing.T) {
	pod := &corev1.Pod{}
	isManaged := func(k string) bool { return true }
	if changed := syncManagedLabels(pod, isManaged, nil); changed {
		t.Error("expected changed=false for nil labels + nil desired")
	}
	if pod.Labels != nil {
		t.Error("nil labels should stay nil")
	}
}

func TestSyncManagedLabels_NilLabelsThenAdd(t *testing.T) {
	pod := &corev1.Pod{}
	isManaged := func(k string) bool { return true }
	desired := map[string]string{"kube-vnet.system/net.x": "both"}
	if changed := syncManagedLabels(pod, isManaged, desired); !changed {
		t.Error("expected changed=true")
	}
	if pod.Labels["kube-vnet.system/net.x"] != "both" {
		t.Errorf("expected label added, got %v", pod.Labels)
	}
}
