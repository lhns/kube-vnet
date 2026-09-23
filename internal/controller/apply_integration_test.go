//go:build integration

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// A namespace whose NetworkPolicies are rejected by admission must see why
// in its own namespace: the member policy's and the baseline's ApplyFailed
// Events land there, although neither policy exists.
func TestIntegration_ApplyFailedVisibleInMemberNamespace(t *testing.T) {
	ctx := context.Background()
	home := uniqueNS(t, "af-home")
	member := uniqueNS(t, "af-member")
	mustCreate(t, makeNamespace(home, nil, nil))
	mustCreate(t, makeNamespace(member, nil, nil))
	denyPoliciesIn(t, member)
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: home},
		Spec: vnetv1alpha1.VirtualNetworkSpec{
			AllowedNamespaces: &vnetv1alpha1.NamespaceSelector{Names: []string{member}},
		},
	})
	mustCreate(t, makePod(member, "p", map[string]string{"kube-vnet/net." + home + ".v": "both"}))

	for _, name := range []string{PolicyName("v", home), BaselinePolicyName} {
		eventually(t, 20*time.Second, func() error {
			var events corev1.EventList
			if err := testClient.List(ctx, &events, client.InNamespace(member)); err != nil {
				return err
			}
			for _, e := range events.Items {
				if e.Reason == EventApplyFailed && e.InvolvedObject.Kind == "NetworkPolicy" && e.InvolvedObject.Name == name {
					if !strings.Contains(e.Message, "denied by test policy") {
						return fmt.Errorf("event lacks the apiserver's reason: %s", e.Message)
					}
					return nil
				}
			}
			// The baseline may have been applied before the policy was
			// active; deleting it makes the next apply meet the policy.
			_ = client.IgnoreNotFound(testClient.Delete(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: member, Name: BaselinePolicyName},
			}))
			return fmt.Errorf("no ApplyFailed Event on NetworkPolicy %s/%s yet", member, name)
		})
	}
}

// denyPoliciesIn rejects NetworkPolicy writes in ns with a
// ValidatingAdmissionPolicy.
func denyPoliciesIn(t *testing.T, ns string) {
	t.Helper()
	fail := admissionregistrationv1.Deny
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-policies-" + ns},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: ptr(admissionregistrationv1.Fail),
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{"networking.k8s.io"}, APIVersions: []string{"v1"},
							Resources: []string{"networkpolicies"},
						},
					},
				}},
			},
			Validations: []admissionregistrationv1.Validation{{Expression: "false", Message: "denied by test policy"}},
		},
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: vap.Name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        vap.Name,
			ValidationActions: []admissionregistrationv1.ValidationAction{fail},
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
			},
		},
	}
	mustCreate(t, vap)
	mustCreate(t, binding)
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(), binding)
		_ = testClient.Delete(context.Background(), vap)
	})
	// The apiserver enforces a new policy only after its informer syncs.
	eventually(t, 20*time.Second, func() error {
		probe := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "probe"}}
		err := testClient.Create(context.Background(), probe)
		if err == nil {
			_ = testClient.Delete(context.Background(), probe)
			return errors.New("policy not enforced yet")
		}
		if !strings.Contains(err.Error(), "denied by test policy") {
			return err
		}
		return nil
	})
}

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
	web := makeLBService(ns, "web")
	web.Annotations = map[string]string{AnnotationApiserverReachable: "true"}
	mustCreate(t, web)

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

	// Both Service-owned kinds, owned by the live Service.
	ownedByWeb := func(build func(*corev1.Service) (*networkingv1.NetworkPolicy, error)) func() (*networkingv1.NetworkPolicy, error) {
		return func() (*networkingv1.NetworkPolicy, error) {
			svc := &corev1.Service{}
			if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web"}, svc); err != nil {
				return nil, err
			}
			desired, err := build(svc)
			if err != nil {
				return nil, err
			}
			return desired, controllerutil.SetControllerReference(svc, desired, testScheme)
		}
	}
	upToDate(extAllowPolicyName(ns, "web"), ownedByWeb(func(svc *corev1.Service) (*networkingv1.NetworkPolicy, error) {
		return buildExternalAllowPolicy(svc, nil)
	}))
	upToDate(apiserverReachablePolicyName(web), ownedByWeb(func(svc *corev1.Service) (*networkingv1.NetworkPolicy, error) {
		return buildApiserverReachablePolicy(svc, nil, []int32{80}, "0.0.0.0/0")
	}))
}
