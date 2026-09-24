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

// applyFailed counts a failed apply of kind in apply_errors_total and reports
// it as an ApplyFailed Warning on obj.
func applyFailed(rec events.EventRecorder, obj runtime.Object, kind, note string, args ...any) {
	applyErrors.WithLabelValues(kind).Inc()
	eventf(rec, obj, corev1.EventTypeWarning, EventApplyFailed, "Apply", note, args...)
}

// policyRestored reports a recreated policy as a PolicyRestored Warning on obj.
func policyRestored(rec events.EventRecorder, obj runtime.Object, note string, args ...any) {
	eventf(rec, obj, corev1.EventTypeWarning, EventPolicyRestored, "Restore", note, args...)
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
// stops wanting it, so a later creation is not taken for a restore. Not on a
// failed apply: the policy is still wanted, and its eventual recreation after
// a delete is a restore.
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

// onceSet remembers which Events were emitted per object, so a state that
// persists across reconciles (a pod's fixed spec, a Service waiting on a
// named port) is reported when it starts rather than on every reconcile.
// Keyed by name, checked against the UID, so a recreated object of the same
// name is reported again.
type onceSet struct {
	mu   sync.Mutex
	seen map[types.NamespacedName]map[string]types.UID
}

// first reports whether reason has not yet been recorded for obj, and marks it.
func (o *onceSet) first(obj client.Object, reason string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[types.NamespacedName]map[string]types.UID{}
	}
	nn := client.ObjectKeyFromObject(obj)
	byReason := o.seen[nn]
	if byReason == nil {
		byReason = map[string]types.UID{}
		o.seen[nn] = byReason
	}
	if uid, ok := byReason[reason]; ok && uid == obj.GetUID() {
		return false
	}
	byReason[reason] = obj.GetUID()
	return true
}

// clear drops reason for obj: the state ended, so its next start is reported.
func (o *onceSet) clear(obj client.Object, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.seen[client.ObjectKeyFromObject(obj)], reason)
}

// forget drops everything recorded for the object nn (it is gone).
func (o *onceSet) forget(nn types.NamespacedName) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.seen, nn)
}
