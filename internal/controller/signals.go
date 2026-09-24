package controller

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// eventf records an Event regarding obj; a nil recorder records nothing.
// The Event lands in obj's namespace, so the people who can read that
// namespace see it. obj need not exist yet: the reference is built from its
// kind, namespace and name.
func eventf(rec events.EventRecorder, obj runtime.Object, eventtype, reason, action, note string, args ...any) {
	if rec == nil {
		return
	}
	rec.Eventf(obj, nil, eventtype, reason, action, note, args...)
}

// policyTracker remembers which policies this process has applied, so an
// apply that has to create one again is a restore (someone deleted it), not
// a first creation. It is in memory: a policy deleted while the operator was
// down is recreated without a PolicyRestored Event.
type policyTracker struct {
	mu   sync.Mutex
	keys map[client.ObjectKey]bool
}

// applied records key as applied and reports whether the apply restored it:
// created now, applied before.
func (t *policyTracker) applied(key client.ObjectKey, created bool) (restored bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.keys == nil {
		t.keys = map[client.ObjectKey]bool{}
	}
	restored = created && t.keys[key]
	t.keys[key] = true
	return restored
}

// forget drops key. Call it when the operator itself deletes the policy or
// stops wanting it, so a later creation is not taken for a restore.
func (t *policyTracker) forget(key client.ObjectKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.keys, key)
}

// forgetNamespace drops the keys in ns that keep doesn't hold (nil drops all).
func (t *policyTracker) forgetNamespace(ns string, keep map[client.ObjectKey]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.keys {
		if k.Namespace == ns && !keep[k] {
			delete(t.keys, k)
		}
	}
}

// podOnce remembers which one-off pod warnings were emitted, so a warning
// about a pod's fixed spec or labels is recorded once per pod rather than on
// every reconcile. Keyed by name, checked against the UID, so a recreated
// pod of the same name is warned again.
type podOnce struct {
	mu   sync.Mutex
	seen map[types.NamespacedName]map[string]types.UID
}

// first reports whether reason has not yet been recorded for pod, and marks it.
func (o *podOnce) first(pod *corev1.Pod, reason string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[types.NamespacedName]map[string]types.UID{}
	}
	nn := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	byReason := o.seen[nn]
	if byReason == nil {
		byReason = map[string]types.UID{}
		o.seen[nn] = byReason
	}
	if uid, ok := byReason[reason]; ok && uid == pod.UID {
		return false
	}
	byReason[reason] = pod.UID
	return true
}

// forget drops everything recorded for the pod nn (it is gone).
func (o *podOnce) forget(nn types.NamespacedName) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.seen, nn)
}
