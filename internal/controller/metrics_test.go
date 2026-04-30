package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TestMetrics_Registered guards against accidental rename or removal: every
// metric name we expose to operators must stay registered.
//
// *Vec metrics aren't visible to Gather() until at least one labeled series
// has been observed, so we touch each Vec with a benign label first.
func TestMetrics_Registered(t *testing.T) {
	reconciliations.WithLabelValues(ResultSuccess).Add(0)
	applyErrors.WithLabelValues(ApplyErrorMembershipPolicy).Add(0)
	membersByNetwork.WithLabelValues("__test__/__test__").Set(0)

	want := []string{
		"kube_vnet_reconciliations_total",
		"kube_vnet_reconcile_duration_seconds",
		"kube_vnet_networks_total",
		"kube_vnet_managed_policies_total",
		"kube_vnet_members_total",
		"kube_vnet_apply_errors_total",
	}
	got := metricNamesFromRegistry(metrics.Registry)
	for _, name := range want {
		if !contains(got, name) {
			t.Errorf("metric %q is not registered", name)
		}
	}
	// Clean up the synthetic series so they don't pollute later tests.
	membersByNetwork.DeleteLabelValues("__test__/__test__")
}

func TestMetrics_ReconcileObservation(t *testing.T) {
	before := testutil.ToFloat64(reconciliations.WithLabelValues(ResultSuccess))
	observeReconcile(time.Now(), nil)
	after := testutil.ToFloat64(reconciliations.WithLabelValues(ResultSuccess))
	if after != before+1 {
		t.Errorf("success counter did not increment: before=%v after=%v", before, after)
	}
}

func TestMetrics_MembersGauge(t *testing.T) {
	setMembers("platform", "payments", 3)
	v := testutil.ToFloat64(membersByNetwork.WithLabelValues("platform/payments"))
	if v != 3 {
		t.Errorf("members gauge=%v want 3", v)
	}
	clearMembers("platform", "payments")
	// After clear, the series should be gone — DeleteLabelValues returns true if removed.
}

// metricNamesFromRegistry walks a prometheus.Gatherer and returns the registered
// metric family names.
func metricNamesFromRegistry(g prometheus.Gatherer) []string {
	mf, err := g.Gather()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(mf))
	for _, f := range mf {
		out = append(out, f.GetName())
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle || strings.HasPrefix(s, needle) {
			return true
		}
	}
	return false
}

