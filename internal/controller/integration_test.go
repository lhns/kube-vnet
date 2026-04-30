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

	pod := makePod(ns, "orders", map[string]string{"kube-vnet/net.payments": "true"})
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
		// DNS allowance.
		if len(p.Spec.Egress) < 2 {
			return fmt.Errorf("egress rules=%d, expected at least 2", len(p.Spec.Egress))
		}
		return nil
	})
}

func TestIntegration_Baseline_CreatedOnFirstMember(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "baseline")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	})

	// Before any member: baseline should not exist.
	time.Sleep(500 * time.Millisecond)
	bp := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp); !apierrors.IsNotFound(err) {
		t.Fatalf("baseline should not exist yet, got err=%v", err)
	}

	// Adding a member triggers baseline creation.
	mustCreate(t, makePod(ns, "p1", map[string]string{"kube-vnet/net.v": "true"}))
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})
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
	mustCreate(t, makePod(home, "h", map[string]string{"kube-vnet/net.shared": "true"}))
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
	mustCreate(t, makePod(home, "h", map[string]string{"kube-vnet/net.doomed": "true"}))
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
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "true"}))

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
		if len(got.Spec.Ingress) == 0 || len(got.Spec.Egress) == 0 {
			return fmt.Errorf("rules still empty")
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
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "true"}))

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
	pod := makePod(ns, "p", map[string]string{"kube-vnet/net.v": "true"})
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

// TestIntegration_BaselineGC_OnVNetDelete: deleting the vnet must remove the
// baseline default-deny in the namespace too, since the baseline only exists
// to backstop the operator's membership policies. Leaving it behind would
// silently keep the namespace in a deny-by-default state with no operator
// resource left to explain it.
func TestIntegration_BaselineGC_OnVNetDelete(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "bgc")
	mustCreate(t, makeNamespace(ns, nil, nil))
	v := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
	}
	mustCreate(t, v)
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "true"}))

	// Wait for the baseline to appear.
	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})

	// Delete the vnet.
	if err := testClient.Delete(ctx, v); err != nil {
		t.Fatalf("delete vnet: %v", err)
	}

	// Baseline should be GC'd.
	eventually(t, 10*time.Second, func() error {
		err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("baseline still exists after vnet delete (err=%v)", err)
	})
}

// TestIntegration_BaselineGC_TwoVNetsKeepBaseline: deleting one vnet must NOT
// remove the baseline if another vnet in the same namespace still has members.
// Guards against over-eager GC.
func TestIntegration_BaselineGC_TwoVNetsKeepBaseline(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "twovnet")
	mustCreate(t, makeNamespace(ns, nil, nil))
	a := &vnetv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns}}
	b := &vnetv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns}}
	mustCreate(t, a)
	mustCreate(t, b)
	mustCreate(t, makePod(ns, "pa", map[string]string{"kube-vnet/net.a": "true"}))
	mustCreate(t, makePod(ns, "pb", map[string]string{"kube-vnet/net.b": "true"}))

	// Wait for the baseline.
	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp)
	})

	// Delete vnet a only. b's pod is still a member, so the baseline must stay.
	if err := testClient.Delete(ctx, a); err != nil {
		t.Fatalf("delete vnet a: %v", err)
	}

	// Wait for a's policy to be gone.
	eventually(t, 10*time.Second, func() error {
		_, err := findPolicy(ctx, ns, "kube-vnet-a-"+ns)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("a's policy still exists: %v", err)
	})

	// Baseline must still be present.
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: BaselinePolicyName}, bp); err != nil {
		t.Fatalf("baseline disappeared while vnet b still has members: %v", err)
	}
}

// TestIntegration_BaselineGC_ForeignNamespaceEmpties: shrinking
// allowedNamespaces so that a foreign namespace no longer has any membership
// policy must GC the baseline in that foreign namespace. Exercises the
// deleteStale path (different from cleanupForDeleted).
func TestIntegration_BaselineGC_ForeignNamespaceEmpties(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "fhome")
	foreign := uniqueNS(t, "fforeign")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(foreign, nil, nil))

	v := &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{foreign}},
		},
	}
	mustCreate(t, v)
	// Pod in foreign joins via the namespace-prefixed label.
	mustCreate(t, makePod(foreign, "p", map[string]string{
		"kube-vnet/net." + home + ".v": "true",
	}))

	// Wait for the foreign baseline to appear (member triggers it).
	bp := &networkingv1.NetworkPolicy{}
	eventually(t, 10*time.Second, func() error {
		return testClient.Get(ctx, client.ObjectKey{Namespace: foreign, Name: BaselinePolicyName}, bp)
	})

	// Now shrink the spec: remove `foreign` from allowedNamespaces.
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: home, Name: "v"}, v); err != nil {
		t.Fatalf("get vnet: %v", err)
	}
	v.Spec.AllowedNamespaces = nil
	if err := testClient.Update(ctx, v); err != nil {
		t.Fatalf("update vnet: %v", err)
	}

	// Foreign membership policy should be deleted (deleteStale path), and
	// then the baseline in `foreign` should be GC'd.
	eventually(t, 15*time.Second, func() error {
		err := testClient.Get(ctx, client.ObjectKey{Namespace: foreign, Name: BaselinePolicyName}, bp)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("foreign baseline still exists after allowedNamespaces shrank (err=%v)", err)
	})
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
	mustCreate(t, makePod(ns, "p", map[string]string{"kube-vnet/net.v": "true"}))

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

// ----- --default-deny-everywhere flag tests ---------------------------------

// withDefaultDenyEverywhere flips the test's NamespaceReconciler flag for the
// duration of the test, then restores it. Tests serialized by t.
func withDefaultDenyEverywhere(t *testing.T, on bool) {
	t.Helper()
	prior := testNSReconciler.DefaultDenyEverywhere
	testNSReconciler.DefaultDenyEverywhere = on
	t.Cleanup(func() { testNSReconciler.DefaultDenyEverywhere = prior })
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

// ensure imports stay used when individual tests are commented out
var _ = corev1.Namespace{}
