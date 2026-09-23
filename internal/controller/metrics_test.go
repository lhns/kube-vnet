package controller

import (
	"slices"
	"testing"
	"time"

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
	defer membersByNetwork.DeleteLabelValues("__test__/__test__")

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got []string
	for _, f := range families {
		got = append(got, f.GetName())
	}
	for _, name := range []string{
		"kube_vnet_reconciliations_total",
		"kube_vnet_reconcile_duration_seconds",
		"kube_vnet_networks_total",
		"kube_vnet_managed_policies_total",
		"kube_vnet_members_total",
		"kube_vnet_apply_errors_total",
	} {
		if !slices.Contains(got, name) {
			t.Errorf("metric %q is not registered", name)
		}
	}
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
	if v := testutil.ToFloat64(membersByNetwork.WithLabelValues("platform/payments")); v != 3 {
		t.Errorf("members gauge=%v want 3", v)
	}
	// A deleted vnet's series must stop being exported.
	clearMembers("platform", "payments")
	if membersByNetwork.DeleteLabelValues("platform/payments") {
		t.Error("clearMembers left the series in place")
	}
}
