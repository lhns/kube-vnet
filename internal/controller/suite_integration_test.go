//go:build integration

package controller

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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// Shared envtest fixture set up by TestMain. All integration tests share one apiserver.
var (
	testEnv                  *envtest.Environment
	testCfg                  *rest.Config
	testClient               client.Client
	testScheme               = runtime.NewScheme()
	testNSReconciler         *NamespaceReconciler // exposed so tests can flip DefaultDenyEverywhere
	testResolutionReconciler *ResolutionReconciler
	testResolutionDefaults   []OperatorMembership
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(vnetv1alpha1.AddToScheme(testScheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}
	testCfg = cfg

	cl, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client.New: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	testClient = cl

	// Build a manager and start the reconciler in a goroutine.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"}, // disable metrics server in tests
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "manager: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	r := &VirtualNetworkReconciler{
		Client:      mgr.GetClient(),
		APIReader:   mgr.GetAPIReader(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("kube-vnet-test"),
		LabelPrefix: DefaultLabelPrefix,
		NSFilter:    NewNamespaceFilter(nil),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup controller: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	testNSReconciler = &NamespaceReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		NSFilter:  NewNamespaceFilter(nil),
	}
	if err := testNSReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup namespace reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	diagReconciler := &JoinLabelDiagnosticReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("kube-vnet-joinlabel-test"),
		LabelPrefix: DefaultLabelPrefix,
		NSFilter:    NewNamespaceFilter(nil),
	}
	if err := diagReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup joinlabel diagnostic reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	resReconciler := &ResolutionReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		NSFilter:         NewNamespaceFilter(nil),
		LabelPrefix:      DefaultLabelPrefix,
		OperatorDefaults: nil,
	}
	if err := resReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup resolution reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	testResolutionReconciler = resReconciler

	sysVnetReconciler := &SystemVnetReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		NSFilter:          NewNamespaceFilter(nil),
		OperatorNamespace: "kube-vnet-system-test",
	}
	if err := sysVnetReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup system vnet reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager.Start: %v\n", err)
		}
	}()

	// Cleanup is consolidated so it runs from every termination path —
	// normal exit, m.Run() panic, or interrupt signal — without leaking the
	// envtest etcd / kube-apiserver children.
	//
	// On Windows, controller-runtime's testEnv.Stop() reliably calls
	// TerminateProcess on the children but a small fraction of runs still
	// leaves a pair behind (suspected race between manager-goroutine
	// teardown and process kill). Belt-and-braces: after Stop(), forcefully
	// taskkill any leftover etcd / kube-apiserver. On Linux this is a
	// silent no-op (taskkill isn't on PATH; the exec.Command call returns
	// an error we discard).
	stop := func() {
		cancel()
		_ = testEnv.Stop()
		if goruntime.GOOS == "windows" {
			for _, name := range []string{"etcd.exe", "kube-apiserver.exe"} {
				_ = exec.Command("taskkill", "/F", "/IM", name).Run()
			}
		}
	}

	// Signal handler for Ctrl+C. On Windows os.Interrupt is delivered for
	// CTRL_C_EVENT / CTRL_BREAK_EVENT; SIGTERM is included for unix shells.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stop()
		os.Exit(130)
	}()

	// Wrap m.Run() so a panic in setup-after-Start or in any Test* still
	// runs `stop()` before the process exits.
	code := func() (rc int) {
		defer stop()
		return m.Run()
	}()
	os.Exit(code)
}

// eventually polls fn until it returns nil or the deadline expires. fn returns
// an error describing the current expectation failure; the most recent error is
// surfaced via t.Fatalf if the deadline expires.
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

// uniqueNS returns a randomized namespace name unique to a test, to keep
// tests independent on the shared apiserver.
func uniqueNS(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%100000)
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := testClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s/%s: %v", obj, obj.GetNamespace(), obj.GetName(), err)
	}
}

func makeNamespace(name string, annotations map[string]string, labels map[string]string) *corev1.Namespace {
	merged := map[string]string{"kubernetes.io/metadata.name": name}
	for k, v := range labels {
		merged[k] = v
	}
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
			Labels:      merged,
		},
	}
}

func makePod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "registry.k8s.io/pause:3.10",
			}},
		},
	}
}

// findPolicy looks up the membership policy for (vnetName, ns).
func findPolicy(ctx context.Context, ns, name string) (*networkingv1.NetworkPolicy, error) {
	p := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		return nil, err
	}
	return p, nil
}

func conditionStatusOf(vnet *vnetv1alpha1.VirtualNetwork, t string) metav1.ConditionStatus {
	for _, c := range vnet.Status.Conditions {
		if c.Type == t {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}

// hasIngressFromKey returns true if the policy's first ingress rule has a peer
// whose podSelector matches Exists on `key`.
func hasIngressFromKey(p *networkingv1.NetworkPolicy, key string) bool {
	if len(p.Spec.Ingress) == 0 {
		return false
	}
	for _, peer := range p.Spec.Ingress[0].From {
		if peer.PodSelector == nil {
			continue
		}
		for _, expr := range peer.PodSelector.MatchExpressions {
			if expr.Key == key && expr.Operator == metav1.LabelSelectorOpExists {
				return true
			}
		}
	}
	return false
}

// ignored to avoid "imported and not used" if a test removes references.
var _ = apierrors.IsNotFound
