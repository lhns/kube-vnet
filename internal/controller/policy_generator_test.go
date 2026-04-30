package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func newVNet(name, ns string, extent vnetv1alpha1.Extent) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
		Spec:       vnetv1alpha1.VirtualNetworkSpec{Extent: extent},
	}
}

func TestGenerate_NoMembers(t *testing.T) {
	out := Generate(GenerateInput{
		VNet:        newVNet("payments", "platform", vnetv1alpha1.ExtentNamespace),
		MembersByNS: nil,
	})
	if len(out.Policies) != 0 {
		t.Fatalf("expected 0 policies, got %d", len(out.Policies))
	}
}

func TestGenerate_NamespaceExtent_SingleNamespace(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("payments", "platform", vnetv1alpha1.ExtentNamespace),
		MembersByNS: map[string][]string{
			"platform": {"orders-1", "orders-2"},
		},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	if p.Namespace != "platform" {
		t.Errorf("namespace=%s want platform", p.Namespace)
	}
	if p.Name != "kube-vnet-payments-platform" {
		t.Errorf("name=%s want kube-vnet-payments-platform", p.Name)
	}
	if p.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("missing managed-by label")
	}
	if p.Labels[LabelNetwork] != "platform.payments" {
		t.Errorf("network label=%s", p.Labels[LabelNetwork])
	}
	if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].Name != "payments" {
		t.Errorf("expected owner ref to payments")
	}
	if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.payments" {
		t.Errorf("podSelector key=%s want kube-vnet/net.payments", got)
	}
	// Ingress, egress + DNS rule.
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Egress) != 2 {
		t.Errorf("ingress=%d egress=%d", len(p.Spec.Ingress), len(p.Spec.Egress))
	}
	dns := p.Spec.Egress[1]
	if len(dns.Ports) != 2 {
		t.Errorf("DNS rule should have 2 ports, got %d", len(dns.Ports))
	}
	foundUDP, foundTCP := false, false
	for _, port := range dns.Ports {
		if port.Protocol != nil {
			if *port.Protocol == corev1.ProtocolUDP {
				foundUDP = true
			}
			if *port.Protocol == corev1.ProtocolTCP {
				foundTCP = true
			}
		}
	}
	if !foundUDP || !foundTCP {
		t.Errorf("DNS allowance must include UDP and TCP")
	}
}

func TestGenerate_ClusterExtent_TwoNamespaces(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "kube-system", vnetv1alpha1.ExtentCluster),
		MembersByNS: map[string][]string{
			"platform": {"a"},
			"webapp":   {"b"},
		},
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(out.Policies))
	}
	for _, p := range out.Policies {
		// Foreign namespace: prefixed selector key.
		wantKey := "kube-vnet/net.kube-system.observability"
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != wantKey {
			t.Errorf("ns=%s podSelector key=%s want %s", p.Namespace, got, wantKey)
		}
		// No owner reference: policy is not in home namespace.
		if len(p.OwnerReferences) != 0 {
			t.Errorf("ns=%s should not have owner ref (cross-namespace)", p.Namespace)
		}
	}
}

func TestGenerate_HomeAndForeignMixed(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "kube-system", vnetv1alpha1.ExtentCluster),
		MembersByNS: map[string][]string{
			"kube-system": {"home-pod"},
			"platform":    {"a"},
		},
	})
	var home, foreign *bool
	for i := range out.Policies {
		p := &out.Policies[i]
		if p.Namespace == "kube-system" {
			if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.observability" {
				t.Errorf("home selector key=%s want bare form", got)
			}
			if len(p.OwnerReferences) != 1 {
				t.Errorf("home policy must own-ref the vnet")
			}
			t := true
			home = &t
		}
		if p.Namespace == "platform" {
			if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.kube-system.observability" {
				t.Errorf("foreign selector key=%s want prefixed form", got)
			}
			t := true
			foreign = &t
		}
	}
	if home == nil || foreign == nil {
		t.Fatalf("expected both home and foreign policies")
	}
}

func TestPolicyName_TruncatesWithHash(t *testing.T) {
	long := strings.Repeat("x", 250)
	got := PolicyName(long, long)
	if len(got) > 253 {
		t.Errorf("len=%d exceeds 253", len(got))
	}
	// Stable: same input → same output.
	if got != PolicyName(long, long) {
		t.Errorf("not deterministic")
	}
}

func TestJoinLabelKey(t *testing.T) {
	if got := JoinLabelKey("kube-vnet/", "platform", "payments", "platform"); got != "kube-vnet/net.payments" {
		t.Errorf("same-ns: %s", got)
	}
	if got := JoinLabelKey("kube-vnet/", "platform", "payments", "webapp"); got != "kube-vnet/net.platform.payments" {
		t.Errorf("foreign: %s", got)
	}
}
