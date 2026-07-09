//go:build integration

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// setClusterBaseline creates the singleton ClusterVirtualNetworkBaseline
// named `default` with the given memberships and registers a t.Cleanup to
// delete it. nil/empty deletes any existing baseline (no-op if absent).
// Replaces the legacy setOperatorDefaults helper from the pre-ADR-0031 era.
func setClusterBaseline(t *testing.T, memberships []vnetv1alpha1.BaselineMembership) {
	t.Helper()
	ctx := context.Background()
	existing := &vnetv1alpha1.ClusterVirtualNetworkBaseline{}
	_ = testClient.Get(ctx, client.ObjectKey{Name: "default"}, existing)
	if existing.Name != "" {
		_ = testClient.Delete(ctx, existing)
	}
	if len(memberships) == 0 {
		return
	}
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       vnetv1alpha1.ClusterVirtualNetworkBaselineSpec{Memberships: memberships},
	}
	if err := testClient.Create(ctx, cb); err != nil {
		t.Fatalf("create cluster baseline: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), cb) })
}

// sysVnetRef builds a baseline membership for a reserved system vnet.
//
// `namespace` is deliberately OMITTED — the recommended form per ADR 0043.
// `cluster` then resolves to the cluster-wide singleton, and `namespace`
// resolves to each pod's own namespace. Naming the operator's namespace here
// (as this helper and the chart both used to) points at a vnet that does not
// exist: the operator's namespace is unmanaged, so it holds no `namespace`
// vnet, and this test suite never seeds a `cluster` vnet there either.
func sysVnetRef(name, dir string) vnetv1alpha1.BaselineMembership {
	return vnetv1alpha1.BaselineMembership{
		VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: name},
		Direction:         dir,
	}
}

// TestIntegration_Resolution_ClusterBaselineStamped: with a cluster baseline
// of `[namespace=default-both, cluster=default-egress]`, a pod in a managed
// namespace gets kube-vnet.system/net.namespace=both and net.cluster=egress
// stamped (default-* prefix consumed during resolution), plus the
// resolved-generation annotation.
func TestIntegration_Resolution_ClusterBaselineStamped(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("namespace", "default-both"),
		sysVnetRef("cluster", "default-egress"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-defaults")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "x"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net."+ns+".namespace"]; got != "both" {
			return fmt.Errorf("namespace label = %q, want both; labels=%v", got, p.Labels)
		}
		if got := p.Labels["kube-vnet.system/net.cluster"]; got != "egress" {
			return fmt.Errorf("cluster label = %q, want egress; labels=%v", got, p.Labels)
		}
		if p.Annotations[AnnotationResolvedGeneration] == "" {
			return fmt.Errorf("resolved-generation annotation missing")
		}
		return nil
	})
}

// TestIntegration_Resolution_PodLabelOverridesDefault: pod-authored
// kube-vnet/net.cluster=both wins over a default-egress cluster baseline.
func TestIntegration_Resolution_PodLabelOverridesDefault(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("cluster", "default-egress"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-override")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.cluster": "both"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.cluster"]; got != "both" {
			return fmt.Errorf("cluster label = %q, want both (pod-label override)", got)
		}
		return nil
	})
}

// TestIntegration_Resolution_NoneOptsOut: pod-authored kube-vnet/net.namespace=none
// strips the inherited cluster-baseline membership; no kube-vnet.system label
// for `namespace` ends up on the pod.
func TestIntegration_Resolution_NoneOptsOut(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("namespace", "default-both"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-optout")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.namespace": "none"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if _, ok := p.Labels["kube-vnet.system/net."+ns+".namespace"]; ok {
			return fmt.Errorf("namespace label should be absent (=none opt-out), got labels=%v", p.Labels)
		}
		return nil
	})
}

// TestIntegration_Resolution_VirtualNetworkBindingStamped: a VNB with
// podSelector matching this pod stamps the system label.
func TestIntegration_Resolution_VirtualNetworkBindingStamped(t *testing.T) {
	setClusterBaseline(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-vnb")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: ns},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-binding", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "payments", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		// Canonical FQ system label (per ADR 0033).
		if got := p.Labels["kube-vnet.system/net."+ns+".payments"]; got != "both" {
			return fmt.Errorf("payments label = %q, want both", got)
		}
		return nil
	})
}

// TestIntegration_Resolution_BindingConflictIntersection: two
// VirtualNetworkBindings in the same NS, with overlapping podSelectors and
// disagreeing directions. Per ADR 0031 the conflict resolves via
// intersection (fail-closed): both ∩ ingress = ingress.
func TestIntegration_Resolution_BindingConflictIntersection(t *testing.T) {
	setClusterBaseline(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-conflict")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})

	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "b-deny", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "ingress",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "a-allow", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net."+ns+".v"]; got != "ingress" {
			return fmt.Errorf("v label = %q, want ingress (intersection of both + ingress per ADR 0031); labels=%v", got, p.Labels)
		}
		return nil
	})
}

// TestIntegration_Resolution_BindingDeletionStripsLabel: when a VNB is
// deleted, the system labels it caused to be stamped are removed from the
// affected pods.
func TestIntegration_Resolution_BindingDeletionStripsLabel(t *testing.T) {
	setClusterBaseline(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-bdel")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	binding := &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "stamper", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	}
	mustCreate(t, binding)
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p"}))

	// Wait for the binding to stamp the label.
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net."+ns+".v"]; got != "both" {
			return fmt.Errorf("not yet stamped: %v", p.Labels)
		}
		return nil
	})

	// Delete the binding; the label should be removed.
	if err := testClient.Delete(ctx, binding); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if _, ok := p.Labels["kube-vnet.system/net."+ns+".v"]; ok {
			return fmt.Errorf("label should have been stripped, got %v", p.Labels)
		}
		return nil
	})
}

// TestIntegration_Resolution_NamespaceBaselineOverridesCluster: cluster
// baseline says cluster=default-egress; namespace baseline overrides to
// cluster=default-both. Pod ends up stamped `cluster=both` (override
// permitted because cluster value was default-*).
func TestIntegration_Resolution_NamespaceBaselineOverridesCluster(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("cluster", "default-egress"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-nsoverride")
	mustCreate(t, makeNamespace(ns, nil, nil))
	nsBaseline := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBaselineSpec{
			Memberships: []vnetv1alpha1.BaselineMembership{
				{
					// namespace omitted: the cluster singleton (ADR 0043).
					VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "cluster"},
					Direction:         "default-both",
				},
			},
		},
	}
	mustCreate(t, nsBaseline)
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), nsBaseline) })

	mustCreate(t, makePod(ns, "p", map[string]string{"app": "x"}))
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net.cluster"]; got != "both" {
			return fmt.Errorf("cluster label = %q, want both (NS baseline override of default-egress)", got)
		}
		return nil
	})
}

// TestIntegration_Resolution_BareNoneBlocksLowerTiers: cluster baseline
// pins cluster=none (bare); pod label tries cluster=both. Override is
// rejected; no system label for cluster on the pod.
func TestIntegration_Resolution_BareNoneBlocksLowerTiers(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("cluster", "none"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-barenone")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.cluster": "both"}))

	// Wait long enough for several reconciles, then assert the pod label
	// did NOT take effect.
	time.Sleep(2 * time.Second)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels["kube-vnet.system/net.cluster"]; ok {
		t.Errorf("cluster system label should be absent (cluster baseline pinned bare none), got %q; labels=%v", got, p.Labels)
	}
}

// TestIntegration_Resolution_BindingLabelConflictIntersection: a VNB and a
// pod label disagree on direction for the same vnet — both at the pod
// tier. Per ADR 0031 they intersect: ingress ∩ egress = none → no system
// label stamped for that vnet.
func TestIntegration_Resolution_BindingLabelConflictIntersection(t *testing.T) {
	setClusterBaseline(t, nil)

	ctx := context.Background()
	ns := uniqueNS(t, "res-bindinglabel")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "selects-p", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "v", Namespace: ns},
			Direction:         "ingress",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	// Pod carries both the binding-selector label AND a kube-vnet/net.v
	// label that disagrees on direction.
	mustCreate(t, makePod(ns, "p", map[string]string{"app": "p", "kube-vnet/net.v": "egress"}))

	time.Sleep(2 * time.Second)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels["kube-vnet.system/net."+ns+".v"]; ok {
		t.Errorf("v system label should be absent (intersection of ingress + egress = none), got %q; labels=%v", got, p.Labels)
	}
}

// TestIntegration_Resolution_PodLabel_NotPermitted_NoStamp covers the bug
// surfaced by the user: a pod in NS X with `kube-vnet/net.<Y>.<vnet>=both`
// where the target vnet exists but its allowedNamespaces doesn't include X.
// The operator MUST NOT stamp `kube-vnet.system/net.<Y>.<vnet>` on the pod
// — the stamp would lie about membership (the membership policy correctly
// excludes the pod regardless).
func TestIntegration_Resolution_PodLabel_NotPermitted_NoStamp(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	nsA := uniqueNS(t, "perm-nsa")
	nsB := uniqueNS(t, "perm-nsb")
	mustCreate(t, makeNamespace(nsA, nil, nil))
	mustCreate(t, makeNamespace(nsB, nil, nil))

	// Vnet in nsB with NO allowedNamespaces → only nsB pods can join.
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: nsB},
	})

	// Pod in nsA tries to join via prefixed label.
	mustCreate(t, makePod(nsA, "p", map[string]string{
		"kube-vnet/net." + nsB + ".private": "both",
	}))

	time.Sleep(2 * time.Second)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: nsA, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels["kube-vnet.system/net."+nsB+".private"]; ok {
		t.Errorf("cross-NS not-permitted vnet should NOT be stamped, got %q on pod; labels=%v", got, p.Labels)
	}
}

// TestIntegration_Resolution_Binding_NotPermitted_NoStamp: a
// VirtualNetworkBinding in nsA references a vnet in nsB whose
// allowedNamespaces doesn't permit nsA. The binding's intent is rejected;
// no stamp on the pod.
func TestIntegration_Resolution_Binding_NotPermitted_NoStamp(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	nsA := uniqueNS(t, "perm-bind-a")
	nsB := uniqueNS(t, "perm-bind-b")
	mustCreate(t, makeNamespace(nsA, nil, nil))
	mustCreate(t, makeNamespace(nsB, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: nsB},
	})
	mustCreate(t, &vnetv1alpha1.VirtualNetworkBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: nsA},
		Spec: vnetv1alpha1.VirtualNetworkBindingSpec{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "private", Namespace: nsB},
			Direction:         "both",
			PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "p"}},
		},
	})
	mustCreate(t, makePod(nsA, "p", map[string]string{"app": "p"}))

	time.Sleep(2 * time.Second)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: nsA, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels["kube-vnet.system/net."+nsB+".private"]; ok {
		t.Errorf("binding-to-not-permitted vnet should NOT be stamped, got %q; labels=%v", got, p.Labels)
	}
}

// TestIntegration_Resolution_VnetMissing_NoStamp: a pod-label references
// a vnet that doesn't exist. No stamp. (The JoinLabelDiagnosticReconciler
// emits a BareJoinLabelVnetNotFound Event; not asserted here since this
// test focuses on the stamping path.)
func TestIntegration_Resolution_VnetMissing_NoStamp(t *testing.T) {
	setClusterBaseline(t, nil)
	ctx := context.Background()

	ns := uniqueNS(t, "perm-missing")
	mustCreate(t, makeNamespace(ns, nil, nil))

	mustCreate(t, makePod(ns, "p", map[string]string{
		"kube-vnet/net.ghost-vnet": "both",
	}))

	time.Sleep(2 * time.Second)
	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels["kube-vnet.system/net."+ns+".ghost-vnet"]; ok {
		t.Errorf("missing-vnet should NOT be stamped, got %q; labels=%v", got, p.Labels)
	}
}

// TestIntegration_Baseline_SystemVnetRef_ForeignNamespace_NoStamp is the
// end-to-end regression lock for ADR 0043.
//
// A VirtualNetworkBaseline pointing the `namespace` system vnet at the
// operator's namespace names a vnet that does not exist (the operator's
// namespace is unmanaged, so SystemVnetReconciler seeds no `namespace` vnet
// there). Before ADR 0043 resolution silently discarded ref.Namespace and
// substituted the pod's own namespace, so this stamped `net.<ns>.namespace`
// and generated a membership policy — a wrong ref that appeared to work.
//
// It must now be honored, found missing, and dropped: no stamp.
func TestIntegration_Baseline_SystemVnetRef_ForeignNamespace_NoStamp(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("cluster", "default-egress"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-foreignref")
	mustCreate(t, makeNamespace(ns, nil, nil))

	nsBaseline := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBaselineSpec{
			Memberships: []vnetv1alpha1.BaselineMembership{
				{
					// The exact shape the chart and our docs used to produce.
					VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{
						Name:      "namespace",
						Namespace: "kube-vnet-system-test",
					},
					Direction: "both",
				},
			},
		},
	}
	mustCreate(t, nsBaseline)
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), nsBaseline) })

	mustCreate(t, makePod(ns, "p", map[string]string{"app": "x"}))

	// Wait for the pod to be resolved at all (the annotation is written on
	// every successful resolution), then assert the stamp is absent.
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if _, ok := p.Annotations[AnnotationResolvedGeneration]; !ok {
			return fmt.Errorf("pod not resolved yet; labels=%v", p.Labels)
		}
		return nil
	})

	p := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got, ok := p.Labels["kube-vnet.system/net."+ns+".namespace"]; ok {
		t.Fatalf("ref.Namespace was silently rewritten to the pod's namespace: "+
			"stamped net.%s.namespace=%q from a ref naming kube-vnet-system-test; labels=%v",
			ns, got, p.Labels)
	}
	if got, ok := p.Labels["kube-vnet.system/net.kube-vnet-system-test.namespace"]; ok {
		t.Fatalf("stamped a membership for a vnet that does not exist: %q; labels=%v", got, p.Labels)
	}
}

// The recommended form (namespace omitted) must still stamp and generate,
// proving the fix doesn't regress the normal path.
func TestIntegration_Baseline_OmittedNamespace_StampsLocalNamespaceVnet(t *testing.T) {
	setClusterBaseline(t, []vnetv1alpha1.BaselineMembership{
		sysVnetRef("cluster", "default-egress"),
	})

	ctx := context.Background()
	ns := uniqueNS(t, "res-omittedref")
	mustCreate(t, makeNamespace(ns, nil, nil))

	nsBaseline := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
		Spec: vnetv1alpha1.VirtualNetworkBaselineSpec{
			Memberships: []vnetv1alpha1.BaselineMembership{
				{
					VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "namespace"},
					Direction:         "both",
				},
			},
		},
	}
	mustCreate(t, nsBaseline)
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), nsBaseline) })

	mustCreate(t, makePod(ns, "p", map[string]string{"app": "x"}))
	eventually(t, 10*time.Second, func() error {
		p := &corev1.Pod{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, p); err != nil {
			return err
		}
		if got := p.Labels["kube-vnet.system/net."+ns+".namespace"]; got != "both" {
			return fmt.Errorf("net.%s.namespace = %q, want both; labels=%v", ns, got, p.Labels)
		}
		return nil
	})
}
