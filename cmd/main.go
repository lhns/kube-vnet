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
		metricsAddr          string
		probeAddr            string
		enableLeaderElect    bool
		labelPrefix          string
		disabledNamespaces   string
		elideBaselineFor   string
		defaultMemberships string
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
	flag.StringVar(&defaultMemberships, "default-memberships", "",
		"comma-separated <vnet>=<direction> pairs that every pod in every "+
			"managed namespace joins by default. The two recognized vnet keys "+
			"are the system vnets `namespace` and `cluster`. Per-pod labels and "+
			"VirtualNetworkBinding rules can add to or override these defaults. "+
			"Per ADR 0030. Example: `namespace=both,cluster=egress`.",
	)
	flag.StringVar(&elideBaselineFor, "elide-baseline-for", "cluster",
		"comma-separated vnet names whose receivers (kube-vnet.system/net.<vnet> "+
			"In [both,ingress]) are excluded from the deny-all baseline. Default "+
			"is `cluster` so the cluster system-vnet's allow-all members don't get "+
			"a redundant baseline policy. See ADR 0030.",
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
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		Scheme:           mgr.GetScheme(),
		NSFilter:         nsFilter,
		BaselineElideFor: splitAndTrim(elideBaselineFor),
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

	parsedDefaults, err := parseDefaultMemberships(defaultMemberships)
	if err != nil {
		setupLog.Error(err, "invalid --default-memberships")
		os.Exit(1)
	}
	resReconciler := &controller.ResolutionReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		NSFilter:         nsFilter,
		LabelPrefix:      labelPrefix,
		OperatorDefaults: parsedDefaults,
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
		"disabled", fmt.Sprintf("%v", disabled),
		"defaultMemberships", defaultMemberships,
		"elideBaselineFor", elideBaselineFor)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// parseDefaultMemberships parses the --default-memberships flag value
// (e.g. `namespace=both,cluster=egress`) into the operator-default-memberships
// list the resolution controller consumes. Empty input → nil. Whitespace
// around tokens is tolerated. Per ADR 0030, only the system vnets `namespace`
// and `cluster` are accepted as keys; user vnets are joined via labels or
// VirtualNetworkBindings.
func parseDefaultMemberships(spec string) ([]controller.OperatorMembership, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var out []controller.OperatorMembership
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("--default-memberships entry %q must be of the form <vnet>=<direction>", pair)
		}
		vnet := strings.TrimSpace(parts[0])
		dirStr := strings.TrimSpace(parts[1])
		if vnet != controller.SystemVnetNamespace && vnet != controller.SystemVnetCluster {
			return nil, fmt.Errorf("--default-memberships vnet %q is not a system vnet name (must be %q or %q)",
				vnet, controller.SystemVnetNamespace, controller.SystemVnetCluster)
		}
		dir, ok := controller.ParseDirection(dirStr)
		if !ok {
			return nil, fmt.Errorf("--default-memberships direction %q is not valid (one of: both, ingress, egress, none)", dirStr)
		}
		out = append(out, controller.OperatorMembership{
			Vnet:      controller.VnetKey(vnet),
			Direction: dir,
		})
	}
	return out, nil
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
