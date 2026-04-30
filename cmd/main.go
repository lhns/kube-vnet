package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vnetv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr           string
		probeAddr             string
		enableLeaderElect     bool
		labelPrefix           string
		excludedNamespaces    string
		defaultDenyEverywhere bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.BoolVar(&enableLeaderElect, "leader-elect", false, "enable leader election for HA")
	flag.StringVar(&labelPrefix, "label-prefix", controller.DefaultLabelPrefix, "label prefix for join labels (must end with /)")
	flag.StringVar(&excludedNamespaces, "excluded-namespaces",
		"kube-system,kube-public,kube-node-lease",
		"comma-separated namespaces excluded from kube-vnet management",
	)
	flag.BoolVar(&defaultDenyEverywhere, "default-deny-everywhere", false,
		"install the kube-vnet-default-deny baseline in every non-excluded, non-disabled namespace, "+
			"even those with no VirtualNetwork members. Cluster-wide default-deny posture. "+
			"Default false — opt in deliberately; flipping this on an existing cluster can break workloads "+
			"that previously relied on default-allow.",
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	excluded := splitAndTrim(excludedNamespaces)
	// Always exclude the operator's own namespace, derived from the downward API.
	if ownNS := os.Getenv("POD_NAMESPACE"); ownNS != "" {
		excluded = append(excluded, ownNS)
	}
	nsFilter := controller.NewNamespaceFilter(excluded)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElect,
		LeaderElectionID:        "kube-vnet.lhns.de",
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	r := &controller.VirtualNetworkReconciler{
		Client:      mgr.GetClient(),
		APIReader:   mgr.GetAPIReader(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("kube-vnet"),
		LabelPrefix: labelPrefix,
		NSFilter:    nsFilter,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller")
		os.Exit(1)
	}

	if err := mgr.Add(&controller.MetricsCollector{Client: mgr.GetClient()}); err != nil {
		setupLog.Error(err, "unable to register metrics collector")
		os.Exit(1)
	}

	nsReconciler := &controller.NamespaceReconciler{
		Client:                mgr.GetClient(),
		APIReader:             mgr.GetAPIReader(),
		Scheme:                mgr.GetScheme(),
		NSFilter:              nsFilter,
		DefaultDenyEverywhere: defaultDenyEverywhere,
	}
	if err := nsReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up namespace reconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add readyz")
		os.Exit(1)
	}

	setupLog.Info("starting kube-vnet operator", "labelPrefix", labelPrefix, "excluded", fmt.Sprintf("%v", excluded))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
