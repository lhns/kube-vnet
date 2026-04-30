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
		metricsAddr             string
		probeAddr               string
		enableLeaderElect       bool
		labelPrefix             string
		excludedNamespaces      string
		ingressIsolationMode    string
		ingressIsolationNone    string
		ingressIsolationNS      string
		ingressIsolationPod     string
		legacyDefaultDenyEvery  bool
		showVersion             bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version info and exit")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.BoolVar(&enableLeaderElect, "leader-elect", false, "enable leader election for HA")
	flag.StringVar(&labelPrefix, "label-prefix", controller.DefaultLabelPrefix, "label prefix for join labels (must end with /)")
	flag.StringVar(&excludedNamespaces, "excluded-namespaces",
		"kube-system,kube-public,kube-node-lease",
		"comma-separated namespaces excluded from kube-vnet management",
	)
	flag.StringVar(&ingressIsolationMode, "ingress-isolation", "none",
		"cluster-wide default ingress-isolation mode (none|namespace|pod). "+
			"Per-namespace annotation kube-vnet/ingress-isolation overrides this; "+
			"the --ingress-isolation-{none,namespace,pod} override flags also win over this default.",
	)
	flag.StringVar(&ingressIsolationNone, "ingress-isolation-none", "",
		"comma-separated namespaces forced to ingress-isolation mode `none` "+
			"(no baseline). Wins over --ingress-isolation; the per-namespace "+
			"annotation still wins over this.",
	)
	flag.StringVar(&ingressIsolationNS, "ingress-isolation-namespace", "",
		"comma-separated namespaces forced to ingress-isolation mode `namespace` "+
			"(baseline allows ingress from same-namespace pods).",
	)
	flag.StringVar(&ingressIsolationPod, "ingress-isolation-pod", "",
		"comma-separated namespaces forced to ingress-isolation mode `pod` "+
			"(baseline denies all ingress).",
	)
	flag.BoolVar(&legacyDefaultDenyEvery, "default-deny-everywhere", false,
		"DEPRECATED: alias for --ingress-isolation=pod. Use --ingress-isolation directly. "+
			"Will be removed in a future release.",
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Printf("kube-vnet %s (commit %s, built %s)\n", version, commit, date)
		os.Exit(0)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	excluded := splitAndTrim(excludedNamespaces)
	if ownNS := os.Getenv("POD_NAMESPACE"); ownNS != "" {
		excluded = append(excluded, ownNS)
	}
	nsFilter := controller.NewNamespaceFilter(excluded)

	// Resolve the ingress-isolation default. Backward compat: the deprecated
	// --default-deny-everywhere=true takes effect only when --ingress-isolation
	// was left at its default value.
	mode, ok := controller.ParseIsolationMode(ingressIsolationMode)
	if !ok {
		setupLog.Info("invalid --ingress-isolation value, refusing to start", "value", ingressIsolationMode)
		os.Exit(1)
	}
	if legacyDefaultDenyEvery && ingressIsolationMode == "none" {
		setupLog.Info("--default-deny-everywhere is deprecated; treating as --ingress-isolation=pod. " +
			"Switch to --ingress-isolation when convenient.")
		mode = controller.IsolationPod
	} else if legacyDefaultDenyEvery {
		setupLog.Info("--default-deny-everywhere set alongside --ingress-isolation; the latter wins. " +
			"Drop --default-deny-everywhere — it's deprecated.")
	}
	nsFilter.DefaultIsolation = mode

	// Populate the override lists. A namespace appearing in more than one
	// list is a configuration error and we refuse to start.
	for _, n := range splitAndTrim(ingressIsolationNone) {
		nsFilter.OverrideIsolationNone[n] = true
	}
	for _, n := range splitAndTrim(ingressIsolationNS) {
		nsFilter.OverrideIsolationNamespace[n] = true
	}
	for _, n := range splitAndTrim(ingressIsolationPod) {
		nsFilter.OverrideIsolationPod[n] = true
	}
	if conflicts := isolationOverrideConflicts(nsFilter); len(conflicts) > 0 {
		setupLog.Info("namespace appears in multiple --ingress-isolation-* override lists; refusing to start",
			"namespaces", conflicts)
		os.Exit(1)
	}

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
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		NSFilter:  nsFilter,
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

	setupLog.Info("starting kube-vnet operator",
		"version", version, "commit", commit, "buildDate", date,
		"labelPrefix", labelPrefix,
		"excluded", fmt.Sprintf("%v", excluded),
		"ingressIsolation", string(mode))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// isolationOverrideConflicts returns any namespace name that appears in more
// than one of the three --ingress-isolation-* override lists.
func isolationOverrideConflicts(f *controller.NamespaceFilter) []string {
	seen := map[string]int{}
	for n := range f.OverrideIsolationNone {
		seen[n]++
	}
	for n := range f.OverrideIsolationNamespace {
		seen[n]++
	}
	for n := range f.OverrideIsolationPod {
		seen[n]++
	}
	var conflicts []string
	for n, count := range seen {
		if count > 1 {
			conflicts = append(conflicts, n)
		}
	}
	return conflicts
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
