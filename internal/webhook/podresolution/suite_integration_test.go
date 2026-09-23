//go:build integration

package podresolution

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/rand"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// Wired exactly as cmd/main.go wires it: one Resolver, two handlers.
	resolver := &controller.Resolver{Reader: mgr.GetClient(), NSFilter: nsFilter}
	decoder := admission.NewDecoder(mgr.GetScheme())
	operatorUser := controller.ServiceAccountUsername("kube-vnet-system-test", "kube-vnet-controller")
	mgr.GetWebhookServer().Register("/mutate-v1-pod", &admission.Webhook{
		Handler: &Mutator{
			Resolver:         resolver,
			Reader:           mgr.GetClient(),
			NSFilter:         nsFilter,
			Decoder:          decoder,
			OperatorUsername: operatorUser,
		},
	})
	mgr.GetWebhookServer().Register("/validate-v1-pod", &admission.Webhook{
		Handler: &Validator{
			Resolver:         resolver,
			Reader:           mgr.GetClient(),
			NSFilter:         nsFilter,
			Decoder:          decoder,
			OperatorUsername: operatorUser,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	// On Windows envtest's Stop() leaves the apiserver and etcd running.
	// Reap only this process's children, never every etcd on the box: another
	// repo's suite is a sibling process and killing its apiserver mid-run
	// looks like a flake over there and is near-impossible to trace back here.
	stop := func() {
		cancel()
		_ = testEnv.Stop()
		if goruntime.GOOS == "windows" {
			script := fmt.Sprintf(
				`Get-CimInstance Win32_Process -Filter "ParentProcessId=%d" | `+
					`Where-Object { $_.Name -in 'etcd.exe','kube-apiserver.exe' } | `+
					`ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
				os.Getpid())
			_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stop()
		os.Exit(130)
	}()

	code := func() (rc int) {
		defer stop()
		return m.Run()
	}()
	os.Exit(code)
}

func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = fn()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("eventually: %v", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func uniqueNS(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + rand.String(5)
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := testClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s/%s: %v", obj, obj.GetNamespace(), obj.GetName(), err)
	}
}

func makeNamespace(name string, labels map[string]string) *corev1.Namespace {
	merged := map[string]string{"kubernetes.io/metadata.name": name}
	for k, v := range labels {
		merged[k] = v
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: merged}}
}

func makePod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "registry.k8s.io/pause:3.10"}},
		},
	}
}
