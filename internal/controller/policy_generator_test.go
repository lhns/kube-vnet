package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

func newVNet(name, ns string) *vnetv1alpha1.VirtualNetwork {
	return &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
	}
}

func TestGenerate_NoMembers(t *testing.T) {
	out := Generate(GenerateInput{
		VNet:        newVNet("payments", "platform"),
		MembersByNS: nil,
	})
	if len(out.Policies) != 0 {
		t.Fatalf("expected 0 policies, got %d", len(out.Policies))
	}
}

func TestGenerate_HomeNamespaceOnly(t *testing.T) {
	out := Generate(GenerateInput{
		VNet:        newVNet("payments", "platform"),
		MembersByNS: map[string][]string{"platform": {"orders-1", "orders-2"}},
	})
	if len(out.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	if p.Namespace != "platform" || p.Name != "kube-vnet-payments-platform" {
		t.Errorf("unexpected name/ns: %s/%s", p.Namespace, p.Name)
	}
	if p.Labels[LabelManagedBy] != LabelManagedByValue || p.Labels[LabelNetwork] != "platform.payments" {
		t.Errorf("unexpected labels: %v", p.Labels)
	}
	if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].Name != "payments" {
		t.Errorf("expected owner ref to payments")
	}
	if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.payments" {
		t.Errorf("podSelector key=%s want kube-vnet/net.payments", got)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Egress) != 2 {
		t.Errorf("ingress=%d egress=%d", len(p.Spec.Ingress), len(p.Spec.Egress))
	}
	dns := p.Spec.Egress[1]
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

func TestGenerate_TwoNamespaces(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "monitoring"),
		MembersByNS: map[string][]string{
			"platform": {"a"},
			"webapp":   {"b"},
		},
	})
	if len(out.Policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(out.Policies))
	}
	for _, p := range out.Policies {
		want := "kube-vnet/net.monitoring.observability"
		if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != want {
			t.Errorf("ns=%s podSelector key=%s want %s", p.Namespace, got, want)
		}
		if len(p.OwnerReferences) != 0 {
			t.Errorf("ns=%s should not have owner ref (cross-namespace)", p.Namespace)
		}
	}
}

func TestGenerate_HomeAndForeignMixed(t *testing.T) {
	out := Generate(GenerateInput{
		VNet: newVNet("observability", "monitoring"),
		MembersByNS: map[string][]string{
			"monitoring": {"home-pod"},
			"platform":   {"a"},
		},
	})
	var sawHome, sawForeign bool
	for i := range out.Policies {
		p := &out.Policies[i]
		switch p.Namespace {
		case "monitoring":
			if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.observability" {
				t.Errorf("home selector key=%s want bare form", got)
			}
			if len(p.OwnerReferences) != 1 {
				t.Errorf("home policy must own-ref the vnet")
			}
			sawHome = true
		case "platform":
			if got := p.Spec.PodSelector.MatchExpressions[0].Key; got != "kube-vnet/net.monitoring.observability" {
				t.Errorf("foreign selector key=%s want prefixed form", got)
			}
			sawForeign = true
		}
	}
	if !sawHome || !sawForeign {
		t.Fatalf("expected both home and foreign policies")
	}
}

func TestPolicyName_TruncatesWithHash(t *testing.T) {
	long := strings.Repeat("x", 250)
	got := PolicyName(long, long)
	if len(got) > 253 {
		t.Errorf("len=%d exceeds 253", len(got))
	}
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

func TestNameRegex(t *testing.T) {
	cases := map[string]bool{
		"payments":     true,
		"a":            true,
		"a-b-c":        true,
		"payments.v2":  false,
		"Payments":     false,
		"-leading":     false,
		"trailing-":    false,
		"":             false,
		"under_score":  false,
	}
	for in, want := range cases {
		got := nameRegex.MatchString(in)
		if got != want {
			t.Errorf("name=%q got=%v want=%v", in, got, want)
		}
	}
}
