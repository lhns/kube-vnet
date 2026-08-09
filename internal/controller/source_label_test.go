package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Kubernetes caps label VALUES at 63 characters. Policy names were capped from
// the start (truncatePolicyName, ADR 0011) but the kube-vnet.system/source label
// value was plain concatenation, so a long enough Service name produced an
// invalid label: the apiserver rejected the apply and the reconciler retried it
// forever. Observed with a Helm-prefixed OpenTelemetry operator webhook Service.

func TestSourceLabelValue_FitsUnderTheLimit(t *testing.T) {
	// Unchanged when it already fits — existing policies must keep their labels
	// on upgrade, so nothing gets rewritten.
	for _, tc := range []struct{ prefix, ns, name, want string }{
		{"svc-", "ns", "traefik", "svc-traefik"},
		{"apiserver-", "ns", "webhook-service", "apiserver-webhook-service"},
		{"svc-", "ns", strings.Repeat("a", 59), "svc-" + strings.Repeat("a", 59)}, // exactly 63
	} {
		if got := SourceLabelValue(tc.prefix, tc.ns, tc.name); got != tc.want {
			t.Errorf("SourceLabelValue(%q,%q,%q) = %q, want %q", tc.prefix, tc.ns, tc.name, got, tc.want)
		}
	}
}

func TestSourceLabelValue_TruncatesOverflowToValidLabel(t *testing.T) {
	// The real-world trigger: a Helm-prefixed OTel operator webhook Service.
	const otel = "opentelemetry-operator-opentelemetry-operator-webhook-service"
	for _, tc := range []struct{ prefix, name string }{
		{"apiserver-", otel},
		{"svc-", otel},
		{"apiserver-", strings.Repeat("x", 253)},
		{"svc-", strings.Repeat("x", 253)},
	} {
		got := SourceLabelValue(tc.prefix, "some-namespace", tc.name)
		if len(got) > 63 {
			t.Errorf("len(%q) = %d, want <= 63", got, len(got))
		}
		if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
			t.Errorf("SourceLabelValue(%q, …) = %q is not a valid label value: %v", tc.prefix, got, errs)
		}
		if !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("%q lost its %q prefix — source-kind filtering depends on it", got, tc.prefix)
		}
	}
}

// Truncation must not collapse distinct Services onto one label, or a delete
// selector would match the wrong policy.
func TestSourceLabelValue_DistinguishesNamesThatTruncateAlike(t *testing.T) {
	base := strings.Repeat("a", 70)
	one := SourceLabelValue("svc-", "ns", base+"-one")
	two := SourceLabelValue("svc-", "ns", base+"-two")
	if one == two {
		t.Fatalf("names differing only past the truncation point collided: %q", one)
	}

	// Same name in different namespaces must also differ: the hash covers
	// namespace/name, matching how policy names disambiguate.
	a := SourceLabelValue("svc-", "ns-a", base)
	b := SourceLabelValue("svc-", "ns-b", base)
	if a == b {
		t.Fatalf("same name in different namespaces collided: %q", a)
	}
}

// The value is written on the policy AND used as a List selector to delete a
// Service's policies. If the two ever disagreed, deletes would match nothing
// and leave orphans behind, so the function must be deterministic.
func TestSourceLabelValue_IsDeterministic(t *testing.T) {
	const name = "opentelemetry-operator-opentelemetry-operator-webhook-service"
	first := SourceLabelValue("apiserver-", "otel", name)
	for i := 0; i < 100; i++ {
		if got := SourceLabelValue("apiserver-", "otel", name); got != first {
			t.Fatalf("not deterministic: %q vs %q", got, first)
		}
	}
}
