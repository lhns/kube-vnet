package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// These tests pin the churn-reduction contract: pod predicates must fire on
// changes to the things kube-vnet reads, not on every update of a pod that
// merely carries a relevant label.

func podWithLabels(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
	}
}

// A pure status update on a join-labelled pod must not enqueue.
func TestJoinLabelChangedPredicate_StatusOnlyUpdate_DoesNotFire(t *testing.T) {
	p := JoinLabelChangedPredicate()
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
	p := JoinLabelChangedPredicate()
	const user, stamp = "kube-vnet/net.payments", "kube-vnet.system/net.shop.payments"
	for _, tc := range []struct {
		name       string
		oldL, newL map[string]string
		wantFire   bool
	}{
		{"user label added", map[string]string{"app": "web"}, map[string]string{"app": "web", user: "both"}, true},
		{"user label removed", map[string]string{user: "both"}, map[string]string{}, true},
		{"user label direction changed", map[string]string{user: "both"}, map[string]string{user: "ingress"}, true},
		// The resolution controller's stamp write; the generator must see it.
		{"system stamp added", map[string]string{user: "both"}, map[string]string{user: "both", stamp: "both"}, true},
		{"system stamp value changed", map[string]string{stamp: "both"}, map[string]string{stamp: "ingress"}, true},
		{"system stamp removed", map[string]string{stamp: "both"}, map[string]string{}, true},
		{"unrelated label changed", map[string]string{user: "both", "version": "1"}, map[string]string{user: "both", "version": "2"}, false},
		{"no kube-vnet labels at all", map[string]string{"app": "web"}, map[string]string{"app": "api"}, false},
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

// The resolution-marker branch. For a pod whose membership is *denied*
// (foreign namespace, typo'd vnet, bad direction) the resolution controller
// writes ONLY the kube-vnet.system/resolved-generation annotation — no system
// stamp label is ever added, and the user label never changes again. A
// label-only diff would drop that update, leaving InvalidJoiners stale until
// the 10-minute resync. The annotation diff makes the vnet re-evaluate exactly
// when resolution finishes.
func TestJoinLabelChangedPredicate_ResolvedGenerationChangeFires(t *testing.T) {
	p := JoinLabelChangedPredicate()

	// Labels identical on both sides — only the resolution annotation moves.
	labels := map[string]string{"kube-vnet/net.payments": "both"}
	oldPod := podWithLabels("shop", "web-0", labels)
	newPod := oldPod.DeepCopy()
	newPod.Annotations = map[string]string{AnnotationResolvedGeneration: "5"}
	newPod.ResourceVersion = "2"

	if !p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
		t.Fatal("resolved-generation annotation change must fire; a denied pod " +
			"gets only this annotation, and dropping it leaves InvalidJoiners stale")
	}

	// And a no-op on the annotation (unchanged) with unchanged labels must NOT.
	same := oldPod.DeepCopy()
	same.Annotations = map[string]string{AnnotationResolvedGeneration: "5"}
	oldPod.Annotations = map[string]string{AnnotationResolvedGeneration: "5"}
	if p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: same}) {
		t.Fatal("identical labels and identical resolved-generation must not fire")
	}
}

// Create/Delete keep membership semantics — those are genuine state changes,
// and the handler needs them to enqueue (or clean up) the vnet.
func TestJoinLabelChangedPredicate_CreateDeleteUnchanged(t *testing.T) {
	p := JoinLabelChangedPredicate()
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
	restarted := mk(8080, corev1.ProtocolTCP)
	restarted.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3}}
	// ADR 0040: hostNetwork pods are out of scope; desiredHostPortKeys skips
	// them, so both sides derive to the empty set.
	hostNetOld, hostNetNew := mk(8080, corev1.ProtocolTCP), mk(9090, corev1.ProtocolTCP)
	hostNetOld.Spec.HostNetwork, hostNetNew.Spec.HostNetwork = true, true

	for _, tc := range []struct {
		name     string
		old, new *corev1.Pod
		wantFire bool
	}{
		{"port swap", mk(8080, corev1.ProtocolTCP), mk(9090, corev1.ProtocolTCP), true},
		{"protocol swap", mk(53, corev1.ProtocolTCP), mk(53, corev1.ProtocolUDP), true},
		{"gaining a hostPort", mk(0, ""), mk(8080, corev1.ProtocolTCP), true},
		{"status-only update", mk(8080, corev1.ProtocolTCP), restarted, false},
		{"hostNetwork pod", hostNetOld, hostNetNew, false},
	} {
		if got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.wantFire {
			t.Errorf("%s: fired=%v, want %v", tc.name, got, tc.wantFire)
		}
	}
}
