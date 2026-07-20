package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// These tests pin the churn-reduction contract: pod predicates must fire on
// *changes* to the things kube-vnet cares about, not on mere membership.
//
// Before this, JoinLabelPodPredicate returned true whenever the pod *carried*
// a join label, so every status heartbeat (restart counts, readiness flips,
// podIP assignment) enqueued a reconcile — each of which ran a cluster-wide
// PodList. A pod restart storm therefore became a reconcile storm.

func podWithLabels(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
	}
}

// REGRESSION LOCK. A pure status update on a join-labelled pod must NOT
// enqueue. This is the storm behaviour.
func TestJoinLabelChangedPredicate_StatusOnlyUpdate_DoesNotFire(t *testing.T) {
	p := JoinLabelChangedPredicate(DefaultLabelPrefix)
	labels := map[string]string{"kube-vnet/net.payments": "both", "app": "web"}

	oldPod := podWithLabels("shop", "web-0", labels)
	newPod := oldPod.DeepCopy()
	// Everything a restart storm actually churns: phase, restart count, IP.
	newPod.Status.Phase = corev1.PodRunning
	newPod.Status.PodIP = "10.1.2.3"
	newPod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 7}}
	newPod.ResourceVersion = "2"

	if p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
		t.Fatal("predicate fired on a status-only update; every pod heartbeat would enqueue a reconcile")
	}
}

// The two-prefix requirement. The generator selects on the operator-stamped
// kube-vnet.system/net.* label, so a diff that only watched the user prefix
// would silently break stamp-driven policy regeneration.
func TestJoinLabelChangedPredicate_FiresOnEitherPrefix(t *testing.T) {
	p := JoinLabelChangedPredicate(DefaultLabelPrefix)

	for _, tc := range []struct {
		name     string
		oldL     map[string]string
		newL     map[string]string
		wantFire bool
	}{
		{
			name:     "user label added",
			oldL:     map[string]string{"app": "web"},
			newL:     map[string]string{"app": "web", "kube-vnet/net.payments": "both"},
			wantFire: true,
		},
		{
			name:     "user label removed",
			oldL:     map[string]string{"kube-vnet/net.payments": "both"},
			newL:     map[string]string{},
			wantFire: true,
		},
		{
			name:     "user label direction changed",
			oldL:     map[string]string{"kube-vnet/net.payments": "both"},
			newL:     map[string]string{"kube-vnet/net.payments": "ingress"},
			wantFire: true,
		},
		{
			// The resolution controller's stamp write. The generator MUST see it.
			name:     "system stamp added",
			oldL:     map[string]string{"kube-vnet/net.payments": "both"},
			newL:     map[string]string{"kube-vnet/net.payments": "both", "kube-vnet.system/net.shop.payments": "both"},
			wantFire: true,
		},
		{
			name:     "system stamp value changed",
			oldL:     map[string]string{"kube-vnet.system/net.shop.payments": "both"},
			newL:     map[string]string{"kube-vnet.system/net.shop.payments": "ingress"},
			wantFire: true,
		},
		{
			name:     "system stamp removed",
			oldL:     map[string]string{"kube-vnet.system/net.shop.payments": "both"},
			newL:     map[string]string{},
			wantFire: true,
		},
		{
			// Unrelated label churn must not enqueue.
			name:     "unrelated label changed",
			oldL:     map[string]string{"kube-vnet/net.payments": "both", "version": "1"},
			newL:     map[string]string{"kube-vnet/net.payments": "both", "version": "2"},
			wantFire: false,
		},
		{
			name:     "no kube-vnet labels at all",
			oldL:     map[string]string{"app": "web"},
			newL:     map[string]string{"app": "api"},
			wantFire: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{
				ObjectOld: podWithLabels("shop", "web-0", tc.oldL),
				ObjectNew: podWithLabels("shop", "web-0", tc.newL),
			})
			if got != tc.wantFire {
				t.Fatalf("Update fired=%v, want %v", got, tc.wantFire)
			}
		})
	}
}

// Create/Delete keep membership semantics — those are genuine state changes,
// and the handler needs them to enqueue (or clean up) the vnet.
func TestJoinLabelChangedPredicate_CreateDeleteUnchanged(t *testing.T) {
	p := JoinLabelChangedPredicate(DefaultLabelPrefix)
	labelled := podWithLabels("shop", "web-0", map[string]string{"kube-vnet/net.payments": "both"})
	plain := podWithLabels("shop", "web-1", map[string]string{"app": "web"})

	if !p.Create(event.CreateEvent{Object: labelled}) {
		t.Error("Create on a join-labelled pod must fire")
	}
	if p.Create(event.CreateEvent{Object: plain}) {
		t.Error("Create on an unlabelled pod must not fire")
	}
	if !p.Delete(event.DeleteEvent{Object: labelled}) {
		t.Error("Delete on a join-labelled pod must fire")
	}
	if p.Delete(event.DeleteEvent{Object: plain}) {
		t.Error("Delete on an unlabelled pod must not fire")
	}
}

// REGRESSION LOCK for the hostPort predicate. The old one asked "does this pod
// have A hostPort?" — a boolean — so swapping 8080->9090 kept it true on both
// sides and the emitted policy would have been left pointing at the old port
// on a status-only requeue. Compare the derived key SET instead.
func TestHostPortChangedPredicate(t *testing.T) {
	p := HostPortChangedPredicate()

	mk := func(port int32, proto corev1.Protocol) *corev1.Pod {
		pod := podWithLabels("shop", "daemon", nil)
		if port != 0 {
			pod.Spec.Containers = []corev1.Container{{
				Name:  "app",
				Ports: []corev1.ContainerPort{{HostPort: port, Protocol: proto}},
			}}
		}
		return pod
	}

	t.Run("port swap fires", func(t *testing.T) {
		if !p.Update(event.UpdateEvent{ObjectOld: mk(8080, corev1.ProtocolTCP), ObjectNew: mk(9090, corev1.ProtocolTCP)}) {
			t.Fatal("8080->9090 must fire; the desired policy changed")
		}
	})
	t.Run("protocol swap fires", func(t *testing.T) {
		if !p.Update(event.UpdateEvent{ObjectOld: mk(53, corev1.ProtocolTCP), ObjectNew: mk(53, corev1.ProtocolUDP)}) {
			t.Fatal("TCP->UDP on the same port must fire")
		}
	})
	t.Run("gaining a hostPort fires", func(t *testing.T) {
		if !p.Update(event.UpdateEvent{ObjectOld: mk(0, ""), ObjectNew: mk(8080, corev1.ProtocolTCP)}) {
			t.Fatal("gaining a hostPort must fire")
		}
	})
	t.Run("status-only update does not fire", func(t *testing.T) {
		oldPod := mk(8080, corev1.ProtocolTCP)
		newPod := oldPod.DeepCopy()
		newPod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3}}
		if p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
			t.Fatal("status-only update must not fire")
		}
	})
	t.Run("hostNetwork pod never fires", func(t *testing.T) {
		// ADR 0040: hostNetwork pods are out of scope; desiredHostPortKeys
		// skips them, so both sides derive to the empty set.
		oldPod, newPod := mk(8080, corev1.ProtocolTCP), mk(9090, corev1.ProtocolTCP)
		oldPod.Spec.HostNetwork, newPod.Spec.HostNetwork = true, true
		if p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
			t.Fatal("hostNetwork pods are out of scope and must not enqueue")
		}
	})
}
