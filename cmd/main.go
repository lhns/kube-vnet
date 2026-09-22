package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/controller"
	"github.com/lhns/kube-vnet/internal/webhook/podresolution"
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
	// Required by the ApiserverReachableReconciler (ADR 0041): APIService
	// + CRD discovery resources aren't in the default client-go scheme.
	utilruntime.Must(apiregistrationv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr         string
		probeAddr           string
		enableLeaderElect   bool
		disabledNamespaces  string
		apiserverSourceCIDR string
		showVersion         bool
		webhookEnabled      bool
		webhookPort         int
		webhookCertDir      string
		serviceAccountName  string
	)
	flag.BoolVar(&showVersion, "version", false, "print version info and exit")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.BoolVar(&enableLeaderElect, "leader-elect", false, "enable leader election for HA")
	flag.StringVar(&disabledNamespaces, "disabled-namespaces",
		"kube-system",
		"comma-separated namespaces the operator never touches (no baseline, no "+
			"membership policies, pods not eligible peers, bindings ignored). "+
			"Mirrors the per-namespace kube-vnet/disabled=true annotation. "+
			"Default protects kube-system (its cluster-critical pods) from "+
			"kube-vnet objects entirely; kube-public/kube-node-lease are podless "+
			"so they are not disabled. Remove a namespace from this list to "+
			"enroll its pods in a vnet (when enrolling kube-system, keep CoreDNS "+
			"reachable — see the chart's dnsCarveout / ADR 0042).",
	)
	flag.StringVar(&apiserverSourceCIDR, "apiserver-source-cidr", "0.0.0.0/0",
		"CIDR allowed as source for auto-allow NetworkPolicies targeting "+
			"Services reached by the apiserver (admission webhooks, "+
			"APIServices, CRD conversion webhooks). Default 0.0.0.0/0 "+
			"matches the cluster's no-NetworkPolicy baseline; narrow to "+
			"your control-plane node CIDR (e.g. 10.1.2.0/24) when the pod "+
			"network is exposed and you want tighter scoping. See ADR 0041.",
	)

	flag.BoolVar(&webhookEnabled, "webhook-enabled", false,
		"serve the pod-resolution admission webhooks (ADR 0034). The mutating "+
			"webhook stamps membership labels during pod admission, closing the "+
			"window in which a starting pod is denied because it is not yet "+
			"stamped; the validating webhook enforces that those labels match "+
			"resolution. Requires serving certificates in --webhook-cert-dir "+
			"and the matching webhook configurations (the chart installs both "+
			"when webhook.enabled=true).",
	)
	flag.StringVar(&serviceAccountName, "service-account-name", "kube-vnet-controller",
		"the operator's own ServiceAccount name. The validating webhook exempts "+
			"this identity so the reconciler's patches are not judged as user "+
			"writes. Must match the chart's serviceAccount name.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "port for the admission webhook server")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"directory holding tls.crt and tls.key for the webhook server")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Printf("kube-vnet %s (commit %s, built %s)\n", version, commit, date)
		os.Exit(0)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Validate --apiserver-source-cidr at startup so a typo in chart values
	// fails fast instead of silently emitting unparseable NetworkPolicies.
	if _, _, err := net.ParseCIDR(apiserverSourceCIDR); err != nil {
		setupLog.Error(err, "invalid --apiserver-source-cidr",
			"value", apiserverSourceCIDR)
		os.Exit(1)
	}

	// POD_NAMESPACE is the operator's release namespace, sourced from the
	// downward API on the Deployment. It anchors the cluster system vnet
	// (per ADR 0033 Amendment), the bare-cluster pod-event routing (per
	// the 059764d fix), and leader-election lease placement. If unset, the
	// operator runs but several features degrade silently — surface a clear
	// warning at startup instead of repeating empty-check logic in each
	// reconciler.
	operatorNS := os.Getenv("POD_NAMESPACE")
	if operatorNS == "" {
		setupLog.Info("POD_NAMESPACE unset; cluster system vnet ownership, " +
			"bare-cluster pod-event routing, and leader-election will " +
			"degrade — set it via the downward API on the operator Deployment")
	}

	disabled := splitAndTrim(disabledNamespaces)
	if operatorNS != "" {
		disabled = append(disabled, operatorNS)
	}
	nsFilter := controller.NewNamespaceFilter(disabled)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElect,
		LeaderElectionID:        "kube-vnet.lhns.de",
		LeaderElectionNamespace: operatorNS,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	r := &controller.VirtualNetworkReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("kube-vnet"),
		NSFilter:          nsFilter,
		OperatorNamespace: operatorNS,
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
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: nsFilter,
	}
	if err := nsReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up namespace reconciler")
		os.Exit(1)
	}

	bindingReconciler := &controller.VirtualNetworkBindingReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("kube-vnet-binding"),
		NSFilter: nsFilter,
	}
	if err := bindingReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up binding reconciler")
		os.Exit(1)
	}

	resReconciler := &controller.ResolutionReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: nsFilter,
		Recorder: mgr.GetEventRecorder("kube-vnet-resolution"),
	}
	if err := resReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up resolution reconciler")
		os.Exit(1)
	}

	sysVnetReconciler := &controller.SystemVnetReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		NSFilter:          nsFilter,
		OperatorNamespace: operatorNS,
	}
	if err := sysVnetReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up system vnet reconciler")
		os.Exit(1)
	}

	extAllowReconciler := &controller.ExternalAllowReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: nsFilter,
		Recorder: mgr.GetEventRecorder("kube-vnet-external-allow"),
	}
	if err := extAllowReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up external-allow reconciler")
		os.Exit(1)
	}

	apiserverReachableReconciler := &controller.ApiserverReachableReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		NSFilter:   nsFilter,
		Recorder:   mgr.GetEventRecorder("kube-vnet-apiserver-reachable"),
		SourceCIDR: apiserverSourceCIDR,
	}
	if err := apiserverReachableReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up apiserver-reachable reconciler")
		os.Exit(1)
	}

	hostPortReconciler := &controller.HostPortReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: nsFilter,
	}
	if err := hostPortReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up host-port reconciler")
		os.Exit(1)
	}

	if webhookEnabled {
		if operatorNS == "" {
			// The validating webhook exempts the operator's own
			// ServiceAccount by username. Without POD_NAMESPACE we cannot
			// build that username, so the operator's own patches would be
			// judged like a user's — and with failurePolicy: Fail that is a
			// cluster-wide outage. Refuse to start instead.
			setupLog.Error(nil, "--webhook-enabled requires POD_NAMESPACE to be set "+
				"(the validating webhook must know the operator's ServiceAccount)")
			os.Exit(1)
		}
		resolver := &controller.Resolver{
			Reader:   mgr.GetClient(),
			NSFilter: nsFilter,
			// No Recorder: admission is not a place to write to the
			// apiserver. The reconciler emits the same diagnostic Events on
			// its own pass moments later.
		}
		decoder := admission.NewDecoder(mgr.GetScheme())
		mgr.GetWebhookServer().Register("/mutate-v1-pod", &admission.Webhook{
			Handler: &podresolution.Mutator{
				Resolver: resolver,
				Reader:   mgr.GetClient(),
				NSFilter: nsFilter,
				Decoder:  decoder,
			},
		})
		mgr.GetWebhookServer().Register("/validate-v1-pod", &admission.Webhook{
			Handler: &podresolution.Validator{
				Resolver:         resolver,
				Reader:           mgr.GetClient(),
				NSFilter:         nsFilter,
				Decoder:          decoder,
				OperatorUsername: controller.ServiceAccountUsername(operatorNS, serviceAccountName),
			},
		})
		setupLog.Info("pod-resolution admission webhooks enabled",
			"port", webhookPort, "certDir", webhookCertDir)
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
