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

// ensure imports stay used when individual tests are commented out
var _ = corev1.Namespace{}
