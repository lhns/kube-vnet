//go:build integration

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func TestIntegration_Create_GeneratesPolicy(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "create")
	mustCreate(t, makeNamespace(ns, nil, nil))

	vnet := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: ns},
	}
	mustCreate(t, vnet)

	pod := makePod(ns, "orders", map[string]string{"kube-vnet/net.payments": "both"})
	mustCreate(t, pod)

	eventually(t, 10*time.Second, func() error {
		p, err := findPolicy(ctx, ns, "kube-vnet-payments-"+ns)
		if err != nil {
			return err
		}
		if p.Labels[LabelManagedBy] != LabelManagedByValue {
			return fmt.Errorf("missing managed-by label")
		}
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.payments" {
			return fmt.Errorf("podSelector key=%s", got)
		}
		// Membership policies are ingress-only (ADR 0025): one ingress allow
		// for vnet peers; no egress section (egress is never restricted).
		if len(p.Spec.Ingress) != 1 || len(p.Spec.Egress) != 0 {
			return fmt.Errorf("expected 1 ingress and 0 egress rules, got ingress=%d egress=%d",
				len(p.Spec.Ingress), len(p.Spec.Egress))
		}
		return nil
	})
}

// TestIntegration_Baseline_NoLongerImplicitOnMember verifies the
// behavior change introduced in ADR 0023: adding a vnet member to a
// namespace no longer implicitly creates the baseline. The baseline is
// now decided by the resolved ingress-isolation mode (default: none).
func TestIntegration_Baseline_NoLongerImplicitOnMember(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "no-implicit")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p1", map[string]string{"kube-vnet/net.v": "both"}))

	// Wait long enough for the membership policy to be applied …
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns)
		return err
	})
	// … and verify the baseline was NOT installed (default isolation mode is none).
	bp := &networkingv1.NetworkPolicy{}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("baseline should NOT exist with default isolation mode (none), got err=%v", err)
	}
}

// TestIntegration_Baseline_AnnotationCreates verifies that setting the
// per-namespace ingress-isolation annotation brings the baseline into
// existence regardless of vnet membership presence.
func TestIntegration_Baseline_AnnotationCreates(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "ann-iso")
	mustCreate(t, makeNamespace(ns, map[string]string{
		"kube-vnet/ingress-isolation": "pod",
	}, nil))

	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})
	// IsolationPod baseline: Ingress only, no allow rules.
	if len(bp.Spec.PolicyTypes) != 1 || bp.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes should be [Ingress], got %v", bp.Spec.PolicyTypes)
	}
}

func TestIntegration_AllowedNamespaces_TwoNamespaces(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "home")
	foreign := uniqueNS(t, "foreign")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(foreign, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{foreign}},
		},
	})
	mustCreate(t, makePod(home, "h", map[string]string{"kube-vnet/net.shared": "both"}))
	mustCreate(t, makePod(foreign, "f", map[string]string{
		"kube-vnet/net." + home + ".shared": "true",
	}))

	eventually(t, 10*time.Second, func() error {
		homeKey := "kube-vnet-shared-" + home
		foreignKey := "kube-vnet-shared-" + foreign
		hp, err := findPolicy(ctx, home, homeKey)
		if err != nil {
			return err
		}
		if got := hp.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.shared" {
			return fmt.Errorf("home key=%s", got)
		}
		fp, err := findPolicy(ctx, foreign, foreignKey)
		if err != nil {
			return err
		}
		if got := fp.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net."+home+".shared" {
			return fmt.Errorf("foreign key=%s", got)
		}
		// Foreign policy must not have an owner ref (cross-namespace not supported).
		if len(fp.OwnerReferences) != 0 {
			return fmt.Errorf("foreign policy has owner ref; expected none")
		}
		return nil
	})
}

func TestIntegration_InvalidJoiner_DegradedCondition(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "ihome")
	other := uniqueNS(t, "iother")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(other, nil, nil))

	// VNet permits no foreign namespaces.
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "strict", Namespace: home},
	})
	// Pod in `other` carries the prefixed join label — should be flagged invalid.
	mustCreate(t, makePod(other, "rogue", map[string]string{
		"kube-vnet/net." + home + ".strict": "true",
	}))

	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "strict"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") != metav1.ConditionTrue {
			return fmt.Errorf("Degraded != True")
		}
		for _, c := range v.Status.Conditions {
			if c.Type == "Degraded" && c.Reason != ReasonInvalidJoiners {
				return fmt.Errorf("reason=%s, want %s", c.Reason, ReasonInvalidJoiners)
			}
		}
		return nil
	})
}

func TestIntegration_Delete_RemovesAllPolicies(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "dhome")
	foreign := uniqueNS(t, "dforeign")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(foreign, nil, nil))

	v := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "doomed", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{foreign}},
		},
	}
	mustCreate(t, v)
	mustCreate(t, makePod(home, "h", map[string]string{"kube-vnet/net.doomed": "both"}))
	mustCreate(t, makePod(foreign, "f", map[string]string{
		"kube-vnet/net." + home + ".doomed": "true",
	}))

	// Wait for both policies to exist.
	eventually(t, 10*time.Second, func() error {
		if _, err := findPolicy(ctx, home, "kube-vnet-doomed-"+home); err != nil {
			return err
		}
		if _, err := findPolicy(ctx, foreign, "kube-vnet-doomed-"+foreign); err != nil {
			return err
		}
		return nil
	})

	if err := testClient.Delete(ctx, v); err != nil {
		t.Fatalf("delete vnet: %v", err)
	}

	// Both policies should disappear.
	eventually(t, 10*time.Second, func() error {
		if _, err := findPolicy(ctx, home, "kube-vnet-doomed-"+home); !apierrors.IsNotFound(err) {
			return fmt.Errorf("home policy still exists: err=%v", err)
		}
		if _, err := findPolicy(ctx, foreign, "kube-vnet-doomed-"+foreign); !apierrors.IsNotFound(err) {
			return fmt.Errorf("foreign policy still exists: err=%v", err)
		}
		return nil
	})
}

func TestIntegration_DriftCorrection(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "drift")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"}))

	policyName := "kube-vnet-v-" + ns
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, policyName)
		return err
	})

	// Clobber the spec.
	p := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: policyName}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	p.Spec.Ingress = nil
	p.Spec.Egress = nil
	if err := testClient.Update(ctx, p); err != nil {
		t.Fatalf("clobber policy: %v", err)
	}

	// Operator should put the rules back.
	eventually(t, 10*time.Second, func() error {
		got, err := findPolicy(ctx, ns, policyName)
		if err != nil {
			return err
		}
		// Membership policies are ingress-only; only ingress should be restored.
		if len(got.Spec.Ingress) == 0 {
			return fmt.Errorf("ingress rules still empty after drift")
		}
		return nil
	})
}

// TestIntegration_DriftCorrection_Membership_DeleteRestores: deleting an
// operator-owned membership policy outright (k9s "delete", `kubectl delete`)
// should be detected and the policy re-created within one reconcile cycle.
// Covers the same path as TestIntegration_DriftCorrection but with delete
// semantics instead of spec-clobber.
func TestIntegration_DriftCorrection_Membership_DeleteRestores(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "drift-mem-del")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"}))

	policyName := "kube-vnet-v-" + ns
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, policyName)
		return err
	})

	// Delete the membership policy outright.
	if err := testClient.Delete(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: policyName},
	}); err != nil {
		t.Fatalf("delete membership policy: %v", err)
	}

	// Operator should re-create it with the right labels.
	eventually(t, 10*time.Second, func() error {
		got, err := findPolicy(ctx, ns, policyName)
		if err != nil {
			return err
		}
		if got.Labels[LabelManagedBy] != LabelManagedByValue {
			return fmt.Errorf("missing managed-by label after re-create: %v", got.Labels)
		}
		if got.Labels[LabelRole] != LabelRoleMembership {
			return fmt.Errorf("role label not %q after re-create: %v", LabelRoleMembership, got.Labels)
		}
		if got.Labels[LabelNetwork] != ns+".v" {
			return fmt.Errorf("network label not %q after re-create: %v", ns+".v", got.Labels)
		}
		return nil
	})
}

// TestIntegration_DriftCorrection_Baseline_DeleteRestores: deleting the
// kube-vnet-default-deny baseline should be detected and the policy
// re-applied. Without the NetworkPolicy watch on NamespaceReconciler this
// test fails — the namespace itself isn't touched, so no Namespace event
// fires and the baseline stays gone.
func TestIntegration_DriftCorrection_Baseline_DeleteRestores(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "drift-base-del")
	mustCreate(t, makeNamespace(ns, map[string]string{
		"kube-vnet/ingress-isolation": "pod",
	}, nil))

	// Wait for the operator to install the baseline.
	eventually(t, 10*time.Second, func() error {
		bp := &networkingv1.NetworkPolicy{}
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})

	// Delete it outright.
	if err := testClient.Delete(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: BaselinePolicyName},
	}); err != nil {
		t.Fatalf("delete baseline: %v", err)
	}

	// Operator should re-apply the baseline with the right labels and shape.
	eventually(t, 10*time.Second, func() error {
		bp := &networkingv1.NetworkPolicy{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp); err != nil {
			return err
		}
		if bp.Labels[LabelManagedBy] != LabelManagedByValue {
			return fmt.Errorf("missing managed-by label after re-create: %v", bp.Labels)
		}
		if bp.Labels[LabelRole] != LabelRoleBaseline {
			return fmt.Errorf("role label not %q after re-create: %v", LabelRoleBaseline, bp.Labels)
		}
		// IsolationPod baseline: PolicyTypes [Ingress], no allow rules.
		if len(bp.Spec.PolicyTypes) != 1 || bp.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
			return fmt.Errorf("policyTypes after re-create: %v want [Ingress]", bp.Spec.PolicyTypes)
		}
		if len(bp.Spec.Ingress) != 0 {
			return fmt.Errorf("expected no ingress allow rules for IsolationPod baseline, got %d", len(bp.Spec.Ingress))
		}
		return nil
	})
}

func TestIntegration_Disabled_NamespaceSkipped(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"}))

	// Wait long enough for several reconciles, then verify nothing was created.
	time.Sleep(2 * time.Second)
	if _, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns); !apierrors.IsNotFound(err) {
		t.Errorf("membership policy should not exist in disabled ns: err=%v", err)
	}
	bp := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp); !apierrors.IsNotFound(err) {
		t.Errorf("baseline should not exist in disabled ns: err=%v", err)
	}
	v := &vnetv1alpha1.VirtualNetwork{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
		t.Fatalf("get vnet: %v", err)
	}
	if conditionStatusOf(v, "Ready") != metav1.ConditionFalse {
		t.Errorf("Ready != False")
	}
}

func TestIntegration_InvalidName_RejectedByAPI(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "badname")
	mustCreate(t, makeNamespace(ns, nil, nil))
	bad := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "has.dots", Namespace: ns},
	}
	err := testClient.Create(ctx, bad)
	if err == nil {
		t.Fatalf("apiserver accepted name with dots; CEL rule not enforcing")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "dns-1123") &&
		!strings.Contains(strings.ToLower(err.Error()), "label") {
		// Just verify the error mentions the constraint somehow.
		t.Logf("rejection error (expected): %v", err)
	}
}

// TestIntegration_AllowedNamespaces_Selector verifies that the Selector matcher
// permits a pod from a labeled foreign namespace and excludes one whose labels
// don't match.
func TestIntegration_AllowedNamespaces_Selector(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "shome")
	prod := uniqueNS(t, "sprod")
	dev := uniqueNS(t, "sdev")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(prod, nil, map[string]string{"tier": "prod"}))
	mustCreate(t, makeNamespace(dev, nil, map[string]string{"tier": "dev"}))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "selvnet", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		},
	})
	// Prod pod is allowed.
	mustCreate(t, makePod(prod, "p", map[string]string{
		"kube-vnet/net." + home + ".selvnet": "true",
	}))
	// Dev pod's join label should be ignored (label doesn't match) and surface as InvalidJoiner.
	mustCreate(t, makePod(dev, "d", map[string]string{
		"kube-vnet/net." + home + ".selvnet": "true",
	}))

	eventually(t, 10*time.Second, func() error {
		// Prod produces a policy with the prefixed key.
		pp, err := findPolicy(ctx, prod, "kube-vnet-selvnet-"+prod)
		if err != nil {
			return err
		}
		want := "kube-vnet/net." + home + ".selvnet"
		if got := pp.Spec.PodSelector.MatchExpressions[0].Key; got != want {
			return fmt.Errorf("prod policy key=%s want %s", got, want)
		}
		// Dev does NOT produce a policy.
		if _, err := findPolicy(ctx, dev, "kube-vnet-selvnet-"+dev); !apierrors.IsNotFound(err) {
			return fmt.Errorf("dev policy should not exist; err=%v", err)
		}
		// Vnet status: Degraded=True with reason InvalidJoiners (the dev pod).
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "selvnet"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") != metav1.ConditionTrue {
			return fmt.Errorf("Degraded != True")
		}
		return nil
	})
}

// TestIntegration_PodRelabeling exercises the handler.Funcs path (ADR 0013):
// removing the join label from a pod must enqueue the formerly-joined vnet
// so its policy stops listing this pod's namespace.
func TestIntegration_PodRelabeling(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "relabel")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	pod := makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"})
	mustCreate(t, pod)

	// Wait for the policy to appear.
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns)
		return err
	})
	// And the vnet to record the member in status.
	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
			return err
		}
		for _, m := range v.Status.Members {
			if m.Namespace == ns {
				for _, name := range m.Pods {
					if name == "p" {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("pod p not yet in members")
	})

	// Strip the join label. The handler.Funcs path must still enqueue v.
	current := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, current); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	delete(current.Labels, "kube-vnet/net.v")
	if err := testClient.Update(ctx, current); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	// Members list should drop the pod.
	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
			return err
		}
		for _, m := range v.Status.Members {
			for _, name := range m.Pods {
				if name == "p" {
					return fmt.Errorf("pod p still listed")
				}
			}
		}
		return nil
	})
}

// TestIntegration_Baseline_AnnotationRemovalRemovesBaseline: removing the
// per-namespace ingress-isolation annotation reverts to the operator default
// (`none` in tests) and the baseline is removed.
func TestIntegration_Baseline_AnnotationRemovalRemovesBaseline(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "ann-revert")
	mustCreate(t, makeNamespace(ns, map[string]string{
		"kube-vnet/ingress-isolation": "pod",
	}, nil))

	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})

	// Strip the annotation.
	current := &corev1.Namespace{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: ns}, current); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	delete(current.Annotations, "kube-vnet/ingress-isolation")
	if err := testClient.Update(ctx, current); err != nil {
		t.Fatalf("update namespace: %v", err)
	}

	eventually(t, 10*time.Second, func() error {
		err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("baseline still exists after annotation removed: err=%v", err)
	})
}

// TestIntegration_Baseline_VNetDeleteDoesNotAffectBaseline: under the new
// decoupled model (ADR 0023), deleting a vnet does not affect the baseline
// — the baseline is owned by NamespaceReconciler and reacts only to the
// resolved IsolationMode, not to membership presence.
func TestIntegration_Baseline_VNetDeleteDoesNotAffectBaseline(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "vdel-keep")
	mustCreate(t, makeNamespace(ns, map[string]string{
		"kube-vnet/ingress-isolation": "pod",
	}, nil))
	v := &vnetv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns}}
	mustCreate(t, v)
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"}))

	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})

	// Delete the vnet.
	if err := testClient.Delete(ctx, v); err != nil {
		t.Fatalf("delete vnet: %v", err)
	}
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("membership policy still exists: %v", err)
	})

	// Baseline should still be there — the annotation hasn't changed.
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp); err != nil {
		t.Fatalf("baseline disappeared after vnet delete (annotation still says ingress-isolation=pod): %v", err)
	}
}

// TestIntegration_PolicyRestoredEvent: deleting an operator-managed
// NetworkPolicy must trigger drift correction AND emit a PolicyRestored event
// on the owning vnet, so accidental or hostile deletion is observable. See
// ADR 0019.
func TestIntegration_PolicyRestoredEvent(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "restore")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"}))

	policyName := "kube-vnet-v-" + ns
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, policyName)
		return err
	})

	// Delete the membership policy. The drift watch should restore it.
	p := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: policyName}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if err := testClient.Delete(ctx, p); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	// Policy comes back.
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, policyName)
		return err
	})

	// And a PolicyRestored event is recorded on the vnet.
	eventually(t, 10*time.Second, func() error {
		var events corev1.EventList
		if err := testClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Reason == EventPolicyRestored && e.InvolvedObject.Kind == "VirtualNetwork" {
				return nil
			}
		}
		return fmt.Errorf("PolicyRestored event not found yet")
	})
}

// TestIntegration_ExcludedNamespace_PodSurfacedInDegraded: a pod in an
// operator-excluded namespace (kube-system by default) carrying the prefixed
// join label is dropped from membership AND surfaced as InvalidJoiner so the
// user can see why it isn't joining.
func TestIntegration_ExcludedNamespace_PodSurfacedInDegraded(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "exhome")
	mustCreate(t, makeNamespace(home, nil, nil))

	// kube-system is excluded by default in the test reconciler? Check:
	// suite_integration_test.go uses NewNamespaceFilter(nil) which has empty
	// excluded set. We need to use an excluded namespace name. We'll build one
	// by adding the kube-vnet/disabled annotation to a test namespace, since
	// that triggers the same NamespaceExcluded path via IsManaged.
	excluded := uniqueNS(t, "exdisabled")
	mustCreate(t, makeNamespace(excluded, map[string]string{"kube-vnet/disabled": "true"}, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "vex", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{All: true},
		},
	})
	mustCreate(t, makePod(excluded, "rogue", map[string]string{
		"kube-vnet/net." + home + ".vex": "true",
	}))

	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "vex"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") != metav1.ConditionTrue {
			return fmt.Errorf("Degraded != True")
		}
		// The Degraded message must mention the excluded pod.
		for _, c := range v.Status.Conditions {
			if c.Type == "Degraded" && c.Reason == ReasonInvalidJoiners &&
				strings.Contains(c.Message, excluded+"/rogue") {
				return nil
			}
		}
		return fmt.Errorf("Degraded condition does not surface %s/rogue: %+v", excluded, v.Status.Conditions)
	})
}

// TestIntegration_AllowedNamespaces_UnlabeledPod_NotAMember: a pod in a
// listed allowed namespace that does NOT carry the join label is not a member.
// allowedNamespaces gates *eligibility to join*, not blanket access. See
// ADR 0005.
func TestIntegration_AllowedNamespaces_UnlabeledPod_NotAMember(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "uhome")
	foreign := uniqueNS(t, "uforeign")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(foreign, nil, nil))

	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{foreign}},
		},
	})

	// Two pods in the listed foreign namespace:
	//   joiner — has the prefixed join label, should become a member.
	//   bystander — has no kube-vnet label, must be ignored.
	mustCreate(t, makePod(foreign, "joiner", map[string]string{
		"kube-vnet/net." + home + ".shared": "true",
	}))
	mustCreate(t, makePod(foreign, "bystander", map[string]string{
		"app": "unrelated",
	}))

	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "shared"}, v); err != nil {
			return err
		}
		// Members for the foreign namespace must contain only "joiner".
		var foreignMembers []string
		for _, m := range v.Status.Members {
			if m.Namespace == foreign {
				foreignMembers = m.Pods
			}
		}
		if len(foreignMembers) != 1 || foreignMembers[0] != "joiner" {
			return fmt.Errorf("foreign members = %v, want [joiner]", foreignMembers)
		}
		// And the policy in foreign uses the prefixed join key as its selector,
		// which by construction won't match bystander's labels.
		fp, err := findPolicy(ctx, foreign, "kube-vnet-shared-"+foreign)
		if err != nil {
			return err
		}
		want := "kube-vnet/net." + home + ".shared"
		if got := fp.Spec.PodSelector.MatchExpressions[0].Key; got != want {
			return fmt.Errorf("foreign policy selector key=%s want %s", got, want)
		}
		return nil
	})
}

// ----- ingress-isolation mode tests ---------------------------------------

// TestIntegration_IngressIsolation_Namespace_BaselineAllowsSameNS:
// kube-vnet/ingress-isolation=namespace puts a baseline that allows ingress
// only from same-namespace pods.
func TestIntegration_IngressIsolation_Namespace_BaselineAllowsSameNS(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "ns-iso")
	mustCreate(t, makeNamespace(ns, map[string]string{
		"kube-vnet/ingress-isolation": "namespace",
	}, nil))

	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})
	if len(bp.Spec.PolicyTypes) != 1 || bp.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes should be [Ingress] only, got %v", bp.Spec.PolicyTypes)
	}
	if len(bp.Spec.Ingress) != 1 {
		t.Fatalf("expected one ingress rule, got %d", len(bp.Spec.Ingress))
	}
	from := bp.Spec.Ingress[0].From[0]
	if from.NamespaceSelector == nil ||
		from.NamespaceSelector.MatchLabels[NamespaceMetadataNameLabel] != ns {
		t.Errorf("ingress peer should select same namespace, got %+v", from)
	}
}

// TestIntegration_IngressIsolation_NoEgressInBaseline: regardless of mode,
// the baseline never restricts egress (ADR 0025).
func TestIntegration_IngressIsolation_NoEgressInBaseline(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "no-egress")
	mustCreate(t, makeNamespace(ns, map[string]string{
		"kube-vnet/ingress-isolation": "pod",
	}, nil))
	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})
	for _, t2 := range bp.Spec.PolicyTypes {
		if t2 == networkingv1.PolicyTypeEgress {
			t.Errorf("baseline must not have Egress in policyTypes (ADR 0025)")
		}
	}
	if len(bp.Spec.Egress) != 0 {
		t.Errorf("baseline egress should be empty, got %+v", bp.Spec.Egress)
	}
}

// ----- direction modes + long-form-in-home tests ---------------------------

// TestIntegration_DirectionEnum_OneOfEach: pods with each of the three
// direction values produce two direction-class self-policies (bidi +
// ingress). The `egress`-only pod gets NO self-policy: it accepts no
// ingress and the operator no longer restricts egress (ADR 0025). It
// still appears in other pods' ingress.from peer lists.
func TestIntegration_DirectionEnum_OneOfEach(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "dir")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "bidi", map[string]string{"kube-vnet/net.v": "both"}))
	mustCreate(t, makePod(ns, "ingr", map[string]string{"kube-vnet/net.v": "ingress"}))
	mustCreate(t, makePod(ns, "egr", map[string]string{"kube-vnet/net.v": "egress"}))

	// Single merged self-policy selecting all receiver-capable members
	// (ADR 0021 Addendum). Both `bidi` and `ingr` pods are covered; `egr`
	// gets no self-policy.
	eventually(t, 10*time.Second, func() error {
		p, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns)
		if err != nil {
			return err
		}
		got := p.Spec.PodSelector.MatchExpressions[0].Values
		want := []string{"true", "both", "ingress"}
		if !equalStringSlice(got, want) {
			return fmt.Errorf("podSelector values=%v want %v", got, want)
		}
		return nil
	})
	// No -ingress / -egress suffixed self-policies after the merge.
	for _, suffix := range []string{"-ingress", "-egress"} {
		if _, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns+suffix); !apierrors.IsNotFound(err) {
			t.Errorf("policy with suffix %q must not exist after the merge: err=%v", suffix, err)
		}
	}
}

// TestIntegration_DirectionEnum_TrueAliasIsBoth: legacy "true" value still
// produces the same single bidi policy with the v1alpha1 name.
func TestIntegration_DirectionEnum_TrueAliasIsBoth(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "alias")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "true"}))

	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns)
		return err
	})
	// And no -ingress / -egress suffixed variants.
	if _, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns+"-ingress"); !apierrors.IsNotFound(err) {
		t.Errorf("ingress policy should not exist for true-only members: %v", err)
	}
}

// TestIntegration_DirectionEnum_UnknownValue_Degraded: a typo in the direction
// value surfaces as InvalidJoiner with reason UnknownDirection.
func TestIntegration_DirectionEnum_UnknownValue_Degraded(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "unknown")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "typo", map[string]string{"kube-vnet/net.v": "bothh"}))

	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") != metav1.ConditionTrue {
			return fmt.Errorf("Degraded != True")
		}
		for _, c := range v.Status.Conditions {
			if c.Type == "Degraded" && strings.Contains(c.Message, ns+"/typo") {
				return nil
			}
		}
		return fmt.Errorf("Degraded message missing %s/typo: %+v", ns, v.Status.Conditions)
	})
}

// TestIntegration_LongForm_InHome: a pod in the home namespace using the
// prefixed form is a member, with a separate -prefixed-suffix policy
// generated.
func TestIntegration_LongForm_InHome(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "longform")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "longform-pod", map[string]string{
		"kube-vnet/net." + ns + ".v": "true",
	}))

	eventually(t, 10*time.Second, func() error {
		// The -prefixed policy is what matches this pod.
		_, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns+"-prefixed")
		return err
	})
	// And status should list the pod as a member.
	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
			return err
		}
		for _, m := range v.Status.Members {
			if m.Namespace == ns {
				for _, name := range m.Pods {
					if name == "longform-pod" {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("longform-pod not in status.members: %+v", v.Status.Members)
	})
}

// TestIntegration_LongForm_BothInHome_Conflict: a pod in the home namespace
// with both forms present and conflicting direction values surfaces as
// ConflictingDirections.
func TestIntegration_LongForm_BothInHome_Conflict(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "conflict")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{
		"kube-vnet/net.v":          "both",
		"kube-vnet/net." + ns + ".v": "ingress",
	}))

	eventually(t, 10*time.Second, func() error {
		v := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
			return err
		}
		if conditionStatusOf(v, "Degraded") != metav1.ConditionTrue {
			return fmt.Errorf("Degraded != True")
		}
		for _, c := range v.Status.Conditions {
			if c.Type == "Degraded" && strings.Contains(c.Message, ns+"/p") {
				return nil
			}
		}
		return fmt.Errorf("Degraded does not surface %s/p: %+v", ns, v.Status.Conditions)
	})
}

// ----- --default-deny-everywhere flag tests ---------------------------------

// withDefaultDenyEverywhere flips the test reconciler's default isolation
// mode for the duration of the test, then restores it. Implemented via the
// new NamespaceFilter.DefaultIsolation field (the legacy boolean flag is
// gone — see ADRs 0023 + 0025).
func withDefaultDenyEverywhere(t *testing.T, on bool) {
	t.Helper()
	prior := testNSReconciler.NSFilter.DefaultIsolation
	if on {
		testNSReconciler.NSFilter.DefaultIsolation = IsolationPod
	} else {
		testNSReconciler.NSFilter.DefaultIsolation = IsolationNone
	}
	t.Cleanup(func() { testNSReconciler.NSFilter.DefaultIsolation = prior })
}

// touchNamespace forces a reconcile of the namespace by issuing a no-op label
// update. Needed because in tests we may flip the flag *after* a namespace was
// created and the watch already fired without our flag being on.
func touchNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: name}, ns); err != nil {
		t.Fatalf("get namespace %s: %v", name, err)
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["kube-vnet-test/touch"] = fmt.Sprintf("%d", time.Now().UnixNano())
	if err := testClient.Update(context.Background(), ns); err != nil {
		t.Fatalf("touch namespace %s: %v", name, err)
	}
}

// TestIntegration_NamespaceOverride_ShieldsFromClusterMode verifies that a
// namespace listed in OverrideIsolationNone does NOT get a baseline even
// when the cluster-wide default is `pod`. This is the operator-side
// equivalent of the chart's namespaceOverrides.none default that protects
// kube-system / kube-public / kube-node-lease.
func TestIntegration_NamespaceOverride_ShieldsFromClusterMode(t *testing.T) {
	ctx := context.Background()
	shielded := uniqueNS(t, "shielded")
	mustCreate(t, makeNamespace(shielded, nil, nil))

	// Flip cluster-wide default to pod and add the test namespace to the
	// none-override list. Restore both at end of test.
	priorMode := testNSReconciler.NSFilter.DefaultIsolation
	testNSReconciler.NSFilter.DefaultIsolation = IsolationPod
	testNSReconciler.NSFilter.OverrideIsolationNone[shielded] = true
	t.Cleanup(func() {
		testNSReconciler.NSFilter.DefaultIsolation = priorMode
		delete(testNSReconciler.NSFilter.OverrideIsolationNone, shielded)
	})

	// Force a reconcile so the override takes effect on the existing namespace.
	touchNamespace(t, shielded)

	// Wait long enough for several reconciles, then assert no baseline.
	time.Sleep(2 * time.Second)
	bp := &networkingv1.NetworkPolicy{}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: shielded, Name: BaselinePolicyName}, bp)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("override-none should keep the baseline out even when default mode is pod, got err=%v", err)
	}

	// Sanity: a sibling namespace WITHOUT the override DOES get the baseline.
	control := uniqueNS(t, "control")
	mustCreate(t, makeNamespace(control, nil, nil))
	eventually(t, 5*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: control, Name: BaselinePolicyName}, bp)
	})
	t.Cleanup(func() {
		_ = testClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: control}})
	})
}

// TestIntegration_DefaultDenyAll_FlagOff_NoBaselineInEmptyNS: regression guard
// for the existing default — flag off, fresh namespace with no vnet, no
// baseline appears.
func TestIntegration_DefaultDenyAll_FlagOff_NoBaselineInEmptyNS(t *testing.T) {
	ctx := context.Background()
	withDefaultDenyEverywhere(t, false)
	ns := uniqueNS(t, "ddaoff")
	mustCreate(t, makeNamespace(ns, nil, nil))

	// Wait long enough for any reconcile to happen, then verify nothing.
	time.Sleep(2 * time.Second)
	bp := &networkingv1.NetworkPolicy{}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("baseline should not exist with flag off: err=%v", err)
	}
}

// TestIntegration_DefaultDenyAll_FlagOn_BaselineEverywhere: flag on, fresh
// namespace with no vnet → baseline appears.
func TestIntegration_DefaultDenyAll_FlagOn_BaselineEverywhere(t *testing.T) {
	ctx := context.Background()
	withDefaultDenyEverywhere(t, true)
	ns := uniqueNS(t, "ddaon")
	mustCreate(t, makeNamespace(ns, nil, nil))
	touchNamespace(t, ns)

	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})
}

// TestIntegration_DefaultDenyAll_FlagOn_DisabledNamespaceSkipped: flag on,
// namespace annotated kube-vnet/disabled=true → no baseline.
func TestIntegration_DefaultDenyAll_FlagOn_DisabledNamespaceSkipped(t *testing.T) {
	ctx := context.Background()
	withDefaultDenyEverywhere(t, true)
	ns := uniqueNS(t, "ddadis")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	touchNamespace(t, ns)

	time.Sleep(2 * time.Second)
	bp := &networkingv1.NetworkPolicy{}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("baseline should not exist in disabled ns even with flag on: err=%v", err)
	}
}

// TestIntegration_DefaultDenyAll_FlagOn_AnnotationFlipsBaselineOff: flag on,
// baseline present, then the disabled annotation gets added → baseline removed.
func TestIntegration_DefaultDenyAll_FlagOn_AnnotationFlipsBaselineOff(t *testing.T) {
	ctx := context.Background()
	withDefaultDenyEverywhere(t, true)
	ns := uniqueNS(t, "ddaflip")
	mustCreate(t, makeNamespace(ns, nil, nil))
	touchNamespace(t, ns)

	// Baseline appears.
	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})

	// Add the disabled annotation.
	current := &corev1.Namespace{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: ns}, current); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations["kube-vnet/disabled"] = "true"
	if err := testClient.Update(ctx, current); err != nil {
		t.Fatalf("annotate namespace: %v", err)
	}

	// Baseline goes away.
	eventually(t, 10*time.Second, func() error {
		err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("baseline still present after disabling: err=%v", err)
	})
}

// ----- JoinLabelDiagnosticReconciler (pod-scoped events; ADR 0027) -------

// hasPodEventWithReason returns nil when an Event of the given reason has
// been recorded against the named pod. Used in eventually() polling.
func hasPodEventWithReason(ctx context.Context, ns, podName, reason string) func() error {
	return func() error {
		var events corev1.EventList
		if err := testClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range events.Items {
			e := &events.Items[i]
			if e.InvolvedObject.Kind == "Pod" && e.InvolvedObject.Name == podName && e.Reason == reason {
				return nil
			}
		}
		return fmt.Errorf("no Event with reason %q on pod %s/%s", reason, ns, podName)
	}
}

// noPodEventWithReason confirms (after a brief settle) that no Event of the
// given reason was recorded against the named pod.
func noPodEventWithReason(t *testing.T, ctx context.Context, ns, podName, reason string) {
	t.Helper()
	time.Sleep(2 * time.Second) // give the controller a chance to (not) emit
	var events corev1.EventList
	if err := testClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	for i := range events.Items {
		e := &events.Items[i]
		if e.InvolvedObject.Kind == "Pod" && e.InvolvedObject.Name == podName && e.Reason == reason {
			t.Fatalf("unexpected Event with reason %q on pod %s/%s: %s", reason, ns, podName, e.Message)
		}
	}
}

// TestIntegration_PodEvent_BareVnetNotFound: a pod in any namespace carrying
// `kube-vnet/net.<X>` (bare form) where no vnet of name X exists in the pod's
// own namespace gets a Warning Event.
func TestIntegration_PodEvent_BareVnetNotFound(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "pe-bare-nf")
	mustCreate(t, makeNamespace(ns, nil, nil))
	// Note: NO vnet created in `ns`. The bare label points at nothing.
	mustCreate(t, makePod(ns, "lonely", map[string]string{"kube-vnet/net.imaginary": "both"}))

	eventually(t, 10*time.Second, hasPodEventWithReason(ctx, ns, "lonely", EventBareJoinLabelVnetNotFound))
}

// TestIntegration_PodEvent_BareWithLocalVnet_NoEvent: bare form is legitimate
// when a vnet of that name exists in the pod's own namespace; no Event.
func TestIntegration_PodEvent_BareWithLocalVnet_NoEvent(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "pe-bare-ok")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "both"}))
	noPodEventWithReason(t, ctx, ns, "p", EventBareJoinLabelVnetNotFound)
}

// TestIntegration_PodEvent_PrefixedVnetNotFound: a pod carrying a prefixed
// label whose named vnet doesn't exist gets a Warning Event.
func TestIntegration_PodEvent_PrefixedVnetNotFound(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "pe-pre-nf")
	mustCreate(t, makeNamespace(ns, nil, nil))
	// Reference a non-existent vnet at "ghost-home/v".
	mustCreate(t, makePod(ns, "p", map[string]string{
		"kube-vnet/net.ghost-home.v": "both",
	}))
	eventually(t, 10*time.Second, hasPodEventWithReason(ctx, ns, "p", EventPrefixedJoinLabelVnetNotFound))
}

// TestIntegration_PodEvent_PrefixedNamespaceNotAllowed: vnet exists at the
// named home but its allowedNamespaces doesn't include the pod's namespace.
func TestIntegration_PodEvent_PrefixedNamespaceNotAllowed(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "pe-na-home")
	other := uniqueNS(t, "pe-na-other")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(other, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: home},
		// allowedNamespaces left nil = home-only; `other` is NOT permitted.
	})
	mustCreate(t, makePod(other, "p", map[string]string{
		"kube-vnet/net." + home + ".v": "both",
	}))
	eventually(t, 10*time.Second, hasPodEventWithReason(ctx, other, "p", EventJoinLabelNamespaceNotAllowed))
}

// TestIntegration_PodEvent_LegitimateMember_NoEvent: prefixed label, vnet
// exists, pod's namespace IS permitted → no diagnostic event.
func TestIntegration_PodEvent_LegitimateMember_NoEvent(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "pe-ok-home")
	other := uniqueNS(t, "pe-ok-other")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(other, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{other}},
		},
	})
	mustCreate(t, makePod(other, "p", map[string]string{
		"kube-vnet/net." + home + ".v": "both",
	}))
	noPodEventWithReason(t, ctx, other, "p", EventPrefixedJoinLabelVnetNotFound)
	noPodEventWithReason(t, ctx, other, "p", EventJoinLabelNamespaceNotAllowed)
}

// TestIntegration_PodEvent_DisabledNamespace_NoEvent: pods in `disabled`
// namespaces are explicit opt-outs; no diagnostics.
func TestIntegration_PodEvent_DisabledNamespace_NoEvent(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "pe-disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.imaginary": "both"}))
	noPodEventWithReason(t, ctx, ns, "p", EventBareJoinLabelVnetNotFound)
}

// TestIntegration_EmptyDirection_NoMember: a pod with `kube-vnet/net.X: ""`
// is NOT a member (empty parses as none, ADR 0027). The vnet membership
// policy's podSelector matches `In [true, both, ingress]` — empty isn't in
// the list — so no policy adds back ingress for this pod.
func TestIntegration_EmptyDirection_NoMember(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "pe-empty")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})
	// Two pods: one with explicit "both" (canonical member), one with "" (not).
	mustCreate(t, makePod(ns, "real", map[string]string{"kube-vnet/net.v": "both"}))
	mustCreate(t, makePod(ns, "empty", map[string]string{"kube-vnet/net.v": ""}))

	// Wait for the membership policy to land for the real member.
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, "kube-vnet-v-"+ns)
		return err
	})

	// Vnet status should list only `real` as a member; `empty` should NOT
	// appear (its label parses as none, so it's not a joiner).
	v := &vnetv1alpha1.VirtualNetwork{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "v"}, v); err != nil {
		t.Fatalf("get vnet: %v", err)
	}
	for _, m := range v.Status.Members {
		for _, p := range m.Pods {
			if p == "empty" {
				t.Fatalf("pod with kube-vnet/net.v=\"\" should NOT be a member; status.members lists it")
			}
		}
	}

	// Belt-and-braces: also confirm `empty` doesn't get a diagnostic event
	// (empty is a deliberate opt-out, not a misuse).
	noPodEventWithReason(t, ctx, ns, "empty", EventBareJoinLabelVnetNotFound)
}

// ensure imports stay used when individual tests are commented out
var _ = strings.HasPrefix
var _ = corev1.Namespace{}
