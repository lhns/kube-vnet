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

// version, commit, date are set at build time via -ldflags. See the Dockerfile
// and Makefile build targets. They show up in `kube-vnet --version` and in the
// "starting kube-vnet operator" log line.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
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
		metricsAddr        string
		probeAddr          string
		enableLeaderElect  bool
		labelPrefix        string
		disabledNamespaces string
		showVersion        bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version info and exit")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.BoolVar(&enableLeaderElect, "leader-elect", false, "enable leader election for HA")
	flag.StringVar(&labelPrefix, "label-prefix", controller.DefaultLabelPrefix, "label prefix for join labels (must end with /)")
	flag.StringVar(&disabledNamespaces, "disabled-namespaces",
		"kube-system,kube-public,kube-node-lease",
		"comma-separated namespaces the operator never touches (no baseline, no "+
			"membership policies, pods not eligible peers, bindings ignored). "+
			"Mirrors the per-namespace kube-vnet/disabled=true annotation. "+
			"Default protects the system namespaces from kube-vnet objects "+
			"entirely; remove a namespace from this list to enroll its pods in "+
			"a vnet.",
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Printf("kube-vnet %s (commit %s, built %s)\n", version, commit, date)
		os.Exit(0)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	disabled := splitAndTrim(disabledNamespaces)
	if ownNS := os.Getenv("POD_NAMESPACE"); ownNS != "" {
		disabled = append(disabled, ownNS)
	}
	nsFilter := controller.NewNamespaceFilter(disabled)


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
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorderFor("kube-vnet"),
		LabelPrefix:       labelPrefix,
		NSFilter:          nsFilter,
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
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
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		NSFilter:  nsFilter,
	}
	if err := nsReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up namespace reconciler")
		os.Exit(1)
	}

	bindingReconciler := &controller.VirtualNetworkBindingReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("kube-vnet-binding"),
		NSFilter: nsFilter,
	}
	if err := bindingReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up binding reconciler")
		os.Exit(1)
	}

	resReconciler := &controller.ResolutionReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		NSFilter:    nsFilter,
		LabelPrefix: labelPrefix,
	}
	if err := resReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up resolution reconciler")
		os.Exit(1)
	}

	sysVnetReconciler := &controller.SystemVnetReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		NSFilter:          nsFilter,
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}
	if err := sysVnetReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up system vnet reconciler")
		os.Exit(1)
	}

	diagReconciler := &controller.JoinLabelDiagnosticReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("kube-vnet-joinlabel"),
		LabelPrefix: labelPrefix,
		NSFilter:    nsFilter,
	}
	if err := diagReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up join-label diagnostic reconciler")
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

	setupLog.Info("starting kube-vnet operator",
		"version", version, "commit", commit, "buildDate", date,
		"labelPrefix", labelPrefix,
		"disabled", fmt.Sprintf("%v", disabled))
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
