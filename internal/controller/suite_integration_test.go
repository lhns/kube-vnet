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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
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
	testEnv    *envtest.Environment
	testCfg    *rest.Config
	testClient client.Client
	testScheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(vnetv1alpha1.AddToScheme(testScheme))
	utilruntime.Must(apiregistrationv1.AddToScheme(testScheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(testScheme))

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
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("kube-vnet-test"),
		NSFilter:          NewNamespaceFilter(nil),
		OperatorNamespace: "kube-vnet-system-test",
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup controller: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	nsReconciler := &NamespaceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: NewNamespaceFilter(nil),
	}
	if err := nsReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup namespace reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	resReconciler := &ResolutionReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: NewNamespaceFilter(nil),
		Recorder: mgr.GetEventRecorder("kube-vnet-resolution"),
	}
	if err := resReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup resolution reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	bindingReconciler := &VirtualNetworkBindingReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("kube-vnet-binding-test"),
		NSFilter: NewNamespaceFilter(nil),
	}
	if err := bindingReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup binding reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	sysVnetReconciler := &SystemVnetReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		NSFilter:          NewNamespaceFilter(nil),
		OperatorNamespace: "kube-vnet-system-test",
	}
	if err := sysVnetReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup system vnet reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	extAllowReconciler := &ExternalAllowReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: NewNamespaceFilter(nil),
		Recorder: mgr.GetEventRecorder("kube-vnet-external-allow-test"),
	}
	if err := extAllowReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup external-allow reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	hostPortReconciler := &HostPortReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NSFilter: NewNamespaceFilter(nil),
	}
	if err := hostPortReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup host-port reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	apiserverReachableReconciler := &ApiserverReachableReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		NSFilter:   NewNamespaceFilter(nil),
		Recorder:   mgr.GetEventRecorder("kube-vnet-apiserver-reachable-test"),
		SourceCIDR: "0.0.0.0/0",
	}
	if err := apiserverReachableReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup apiserver-reachable reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager.Start: %v\n", err)
		}
	}()

	// stop runs on every exit path (normal, panic, interrupt). On Windows,
	// testEnv.Stop() can't signal its children and leaves etcd and
	// kube-apiserver running, so they are then killed by parent PID. Not by
	// image name: that would also kill another suite's apiserver on the same
	// machine, which looks like a flake over there.
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

// findPolicy gets the NetworkPolicy ns/name.
func findPolicy(ctx context.Context, ns, name string) (*networkingv1.NetworkPolicy, error) {
	p := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		return nil, err
	}
	return p, nil
}

// waitForPolicy polls until the NetworkPolicy ns/name exists and returns it.
func waitForPolicy(t *testing.T, ns, name string, timeout time.Duration) *networkingv1.NetworkPolicy {
	t.Helper()
	pol := &networkingv1.NetworkPolicy{}
	eventually(t, timeout, func() error {
		return testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, pol)
	})
	return pol
}

// waitForPolicyAbsent polls until the NetworkPolicy ns/name is gone.
func waitForPolicyAbsent(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, func() error {
		_, err := findPolicy(context.Background(), ns, name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("policy %s/%s still exists", ns, name)
	})
}

// assertPolicyStaysAbsent waits out window, then fails if the NetworkPolicy
// ns/name exists. "Nothing was created" has no event to poll for, so this is
// the one place a fixed wait is correct.
func assertPolicyStaysAbsent(t *testing.T, ns, name string, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	if _, err := findPolicy(context.Background(), ns, name); !apierrors.IsNotFound(err) {
		t.Errorf("policy %s/%s should not exist: err=%v", ns, name, err)
	}
}

// updateNamespace applies mutate to the latest namespace, retrying on conflict.
func updateNamespace(t *testing.T, name string, mutate func(*corev1.Namespace)) {
	t.Helper()
	eventually(t, 5*time.Second, func() error {
		ns := &corev1.Namespace{}
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: name}, ns); err != nil {
			return err
		}
		mutate(ns)
		return testClient.Update(context.Background(), ns)
	})
}

// updateService applies mutate to the latest Service, retrying on conflict.
func updateService(t *testing.T, ns, name string, mutate func(*corev1.Service)) {
	t.Helper()
	eventually(t, 5*time.Second, func() error {
		svc := &corev1.Service{}
		if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, svc); err != nil {
			return err
		}
		mutate(svc)
		return testClient.Update(context.Background(), svc)
	})
}

func conditionStatusOf(vnet *vnetv1alpha1.VirtualNetwork, t string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(vnet.Status.Conditions, t); c != nil {
		return c.Status
	}
	return metav1.ConditionUnknown
}

// conditionReason returns the reason of condition t, or "" if it is not set.
func conditionReason(conds []metav1.Condition, t string) string {
	if c := meta.FindStatusCondition(conds, t); c != nil {
		return c.Reason
	}
	return ""
}
