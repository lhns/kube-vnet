//go:build integration

package podresolution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/controller"
	"github.com/lhns/kube-vnet/internal/testutil"
)

// This suite lives here rather than in internal/controller because the
// handlers import that package: a test in `package controller` that imported
// them back would be an import cycle.
//
// It runs both admission paths against a real apiserver and the
// ResolutionReconciler alongside them, so the webhook path and the controller
// path can be compared on the same cluster.
var (
	testEnv    *envtest.Environment
	testCfg    *rest.Config
	testClient client.Client
	itScheme   = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	utilruntime.Must(clientgoscheme.AddToScheme(itScheme))
	utilruntime.Must(vnetv1alpha1.AddToScheme(itScheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(itScheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		// Declared in-memory rather than loaded from rendered YAML so the
		// fixture needs neither helm on PATH nor a generated file to stay in
		// sync. envtest rewrites clientConfig to its local serving endpoint
		// and injects the CA it generates.
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			MutatingWebhooks:   []*admissionregistrationv1.MutatingWebhookConfiguration{testMutatingWebhook()},
			ValidatingWebhooks: []*admissionregistrationv1.ValidatingWebhookConfiguration{testValidatingWebhook()},
		},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}
	testCfg = cfg

	cl, err := client.New(cfg, client.Options{Scheme: itScheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client.New: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	testClient = cl

	whOpts := testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 itScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    whOpts.LocalServingHost,
			Port:    whOpts.LocalServingPort,
			CertDir: whOpts.LocalServingCertDir,
		}),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "manager: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	nsFilter := controller.NewNamespaceFilter(nil)

	// The resolution controller runs too: the negative control asserts that
	// without the webhook the stamp still arrives, just late.
	resolutionReconciler := &controller.ResolutionReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: nsFilter,
	}
	if err := resolutionReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup resolution reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	Register(mgr.GetWebhookServer(), Deps{
		Resolver:         &controller.Resolver{Reader: mgr.GetClient(), NSFilter: nsFilter},
		Reader:           mgr.GetClient(),
		NSFilter:         nsFilter,
		Decoder:          admission.NewDecoder(mgr.GetScheme()),
		OperatorUsername: controller.ServiceAccountUsername("kube-vnet-system-test", "kube-vnet-controller"),
		NetworkWait:      &NetworkWaitConfig{Image: testNetworkWaitImage, Beacons: "beacons.test.svc:9444"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	os.Exit(testutil.Run(m, func() {
		cancel()
		testutil.StopEnv(testEnv)
	}))
}

// Shared with the controller suite through internal/testutil.
var (
	eventually    = testutil.Eventually
	uniqueNS      = testutil.UniqueNS
	makePod       = testutil.Pod
	makeNamespace = testutil.Namespace
)

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	testutil.MustCreate(t, testClient, obj)
}
