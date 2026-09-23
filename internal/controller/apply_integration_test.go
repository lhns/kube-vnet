//go:build integration

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// applyPolicy skips the write when the live policy equals the desired one.
// That only pays off if a policy read back from a real apiserver, after
// defaulting, still equals what its builder produces; otherwise every
// reconcile would patch as before. Checked for each policy kind.
func TestIntegration_ConvergedPoliciesMatchTheirBuilders(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, "apply-noop")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: ns}})
	mustCreate(t, makePod(ns, "web-0", map[string]string{"kube-vnet/net.payments": "both", "app": "web"}))
	mustCreate(t, makeHostPortPod(ns, "hp", 18090, corev1.ProtocolTCP))
	mustCreate(t, makeLBService(ns, "web"))

	upToDate := func(name string, desired func() (*networkingv1.NetworkPolicy, error)) {
		t.Helper()
		eventually(t, 15*time.Second, func() error {
			live, err := findPolicy(ctx, ns, name)
			if err != nil {
				return err
			}
			want, err := desired()
			if err != nil {
				return err
			}
			if !policyUpToDate(live, want) {
				return errors.New("live policy differs from its builder output:\nlive:    " +
					live.Spec.String() + "\ndesired: " + want.Spec.String())
			}
			return nil
		})
	}

	upToDate(BaselinePolicyName, func() (*networkingv1.NetworkPolicy, error) {
		return DesiredBaseline(ns), nil
	})

	upToDate(PolicyName("payments", ns), func() (*networkingv1.NetworkPolicy, error) {
		vnet := &vnetv1alpha1.VirtualNetwork{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "payments"}, vnet); err != nil {
			return nil, err
		}
		r := &VirtualNetworkReconciler{Client: testClient, NSFilter: NewNamespaceFilter(nil)}
		members, _, err := r.discoverMembers(ctx, vnet)
		if err != nil {
			return nil, err
		}
		out := Generate(GenerateInput{VNet: vnet, MembersByNS: members})
		if len(out.Policies) != 1 {
			return nil, errors.New("pod not a member yet")
		}
		return &out.Policies[0], nil
	})

	upToDate(hostPortPolicyName(ns, hostPortKey{port: 18090, protocol: corev1.ProtocolTCP}), func() (*networkingv1.NetworkPolicy, error) {
		return buildHostPortPolicy(ns, hostPortKey{port: 18090, protocol: corev1.ProtocolTCP}), nil
	})

	upToDate(extAllowPolicyName(ns, "web"), func() (*networkingv1.NetworkPolicy, error) {
		svc := &corev1.Service{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web"}, svc); err != nil {
			return nil, err
		}
		desired, err := buildExternalAllowPolicy(svc, nil)
		if err != nil {
			return nil, err
		}
		return desired, controllerutil.SetControllerReference(svc, desired, testScheme)
	})
}
