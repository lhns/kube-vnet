package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Apply error kinds — values for the "kind" label on applyErrors.
const (
	ApplyErrorMembershipPolicy = "membership_policy"
	ApplyErrorBaseline         = "baseline"
)

// Reconcile result label values for reconciliations.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

// Custom Prometheus metrics for kube-vnet. Registered with controller-runtime's
// shared metrics registry so they're scraped on the manager's metrics endpoint.
var (
	reconciliations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kube_vnet_reconciliations_total",
		Help: "Total VirtualNetwork reconciliations by result (success|error).",
	}, []string{"result"})

	reconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kube_vnet_reconcile_duration_seconds",
		Help:    "Duration of VirtualNetwork reconcile loops in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	networksTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kube_vnet_networks_total",
		Help: "Number of VirtualNetwork resources observed in the cluster.",
	})

	managedPolicies = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kube_vnet_managed_policies_total",
		Help: "Number of NetworkPolicies currently managed by kube-vnet.",
	})

	membersByNetwork = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kube_vnet_members_total",
		Help: "Number of pod members per VirtualNetwork (label: <homeNamespace>/<name>).",
	}, []string{"network"})

	applyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kube_vnet_apply_errors_total",
		Help: "Total NetworkPolicy apply errors by kind (membership_policy|baseline).",
	}, []string{"kind"})
)

func init() {
	metrics.Registry.MustRegister(
		reconciliations,
		reconcileDuration,
		networksTotal,
		managedPolicies,
		membersByNetwork,
		applyErrors,
	)
}

// observeReconcile records the outcome of a reconcile in metrics.
func observeReconcile(start time.Time, err error) {
	reconcileDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		reconciliations.WithLabelValues(ResultError).Inc()
		return
	}
	reconciliations.WithLabelValues(ResultSuccess).Inc()
}

// setMembers updates the per-vnet member-count gauge.
func setMembers(homeNS, name string, count int) {
	membersByNetwork.WithLabelValues(homeNS + "/" + name).Set(float64(count))
}

// clearMembers removes the gauge series for a deleted vnet so it stops being scraped.
func clearMembers(homeNS, name string) {
	membersByNetwork.DeleteLabelValues(homeNS + "/" + name)
}

// MetricsCollector is a controller-runtime Runnable that periodically updates
// gauges that are best computed cluster-wide rather than per-reconcile:
//   - kube_vnet_networks_total
//   - kube_vnet_managed_policies_total
//
// Doing this off a 30-second tick avoids biasing toward whichever VirtualNetwork
// just reconciled and keeps the gauge cost off the hot path.
type MetricsCollector struct {
	Client   client.Client
	Interval time.Duration
}

// Start runs until the context is canceled. Implements manager.Runnable.
func (m *MetricsCollector) Start(ctx context.Context) error {
	if m.Interval == 0 {
		m.Interval = 30 * time.Second
	}
	logger := log.FromContext(ctx).WithName("metrics-collector")
	t := time.NewTicker(m.Interval)
	defer t.Stop()

	// Run once immediately so gauges are populated at startup.
	m.collect(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			m.collect(ctx, logger)
		}
	}
}

func (m *MetricsCollector) collect(ctx context.Context, logger logr.Logger) {
	var vnets vnetv1alpha1.VirtualNetworkList
	if err := m.Client.List(ctx, &vnets); err != nil {
		logger.Error(err, "list VirtualNetworks for metrics")
	} else {
		networksTotal.Set(float64(len(vnets.Items)))
	}

	var policies networkingv1.NetworkPolicyList
	if err := m.Client.List(ctx, &policies, client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
	}); err != nil {
		logger.Error(err, "list managed NetworkPolicies for metrics")
		return
	}
	managedPolicies.Set(float64(len(policies.Items)))
}
