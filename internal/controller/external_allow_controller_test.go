package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/util/workqueue"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// svc returns a minimal Service skeleton; specific fields overridden per-test.
func svc(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func TestBuildExternalAllowPolicy_LoadBalancer_NumericPort(t *testing.T) {
	s := svc("traefik", "traefik")
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol == nil {
		t.Fatal("expected policy, got nil")
	}
	if pol.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("missing managed-by label: %v", pol.Labels)
	}
	if pol.Labels[LabelRole] != LabelRoleExternalAllow {
		t.Errorf("wrong role label: %q", pol.Labels[LabelRole])
	}
	if pol.Labels[LabelSource] != "svc-traefik" {
		t.Errorf("wrong source label: %q", pol.Labels[LabelSource])
	}
	if pol.Labels[LabelSourceKind] != LabelSourceKindService {
		t.Errorf("wrong source-kind label: %q", pol.Labels[LabelSourceKind])
	}
	if got := pol.Spec.PodSelector.MatchLabels["app"]; got != "traefik" {
		t.Errorf("podSelector.matchLabels[app] = %q, want traefik", got)
	}
	if len(pol.Spec.Ingress) != 1 || len(pol.Spec.Ingress[0].From) != 1 ||
		pol.Spec.Ingress[0].From[0].IPBlock == nil {
		t.Fatalf("expected one from-rule with ipBlock, got %+v", pol.Spec.Ingress)
	}
	if pol.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("ipBlock cidr = %q, want 0.0.0.0/0", pol.Spec.Ingress[0].From[0].IPBlock.CIDR)
	}
	if len(pol.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("expected one port, got %d", len(pol.Spec.Ingress[0].Ports))
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 80 {
		t.Errorf("port = %d, want 80", got)
	}
	if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", pol.Spec.PolicyTypes)
	}
}

func TestBuildExternalAllowPolicy_NodePort_TargetPortToPodSide(t *testing.T) {
	// Allowed port must be the pod-side targetPort, not the Service Port or
	// the nodePort. By the time external traffic reaches the pod, kube-proxy
	// has DNAT'd node:nodePort → pod:targetPort.
	s := svc("api", "api")
	s.Spec.Type = corev1.ServiceTypeNodePort
	s.Spec.Ports = []corev1.ServicePort{{
		Port:       80,
		TargetPort: intstr.FromInt32(8080),
		NodePort:   32100,
		Protocol:   corev1.ProtocolTCP,
	}}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8080 {
		t.Errorf("port = %d, want 8080 (targetPort, not 80=port or 32100=nodePort)", got)
	}
}

func TestBuildExternalAllowPolicy_NodePort_NamedPort_PodPresent(t *testing.T) {
	s := svc("api", "api")
	s.Spec.Type = corev1.ServiceTypeNodePort
	s.Spec.Ports = []corev1.ServicePort{{
		Port: 80, TargetPort: intstr.FromString("http-port"),
	}}
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Ports: []corev1.ContainerPort{{Name: "http-port", ContainerPort: 8443}},
			}},
		},
	}}
	pol, err := buildExternalAllowPolicy(s, pods)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8443 {
		t.Errorf("named port resolved to %d, want 8443", got)
	}
}

// Backing pods may map one port name to different numbers, e.g. mid-rollout
// after a containerPort change; the Service routes to each pod's own number,
// so every one must be allowed, whatever order the pods are listed in.
func TestBuildExternalAllowPolicy_NamedPort_PodsDisagree_AllowsEach(t *testing.T) {
	s := svc("api", "api")
	s.Spec.Ports = []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}}
	pod := func(port int32) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			}}},
		}
	}
	for _, pods := range [][]corev1.Pod{
		{pod(8080), pod(9090), pod(8080)},
		{pod(9090), pod(8080)},
	} {
		pol, err := buildExternalAllowPolicy(s, pods)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		var got []int
		for _, p := range pol.Spec.Ingress[0].Ports {
			got = append(got, p.Port.IntValue())
		}
		if !slices.Equal(got, []int{8080, 9090}) {
			t.Errorf("ports = %v, want [8080 9090]", got)
		}
	}
}

func TestBuildExternalAllowPolicy_DuplicateTargetPort_EmittedOnce(t *testing.T) {
	s := svc("api", "api")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "a", Port: 80, TargetPort: intstr.FromInt32(8080)},
		{Name: "b", Port: 8080},
	}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := len(pol.Spec.Ingress[0].Ports); got != 1 {
		t.Errorf("got %d ports, want 1: %+v", got, pol.Spec.Ingress[0].Ports)
	}
}

func TestBuildExternalAllowPolicy_NodePort_NamedPort_NoPodYet(t *testing.T) {
	s := svc("api", "api")
	s.Spec.Ports = []corev1.ServicePort{{
		Port: 80, TargetPort: intstr.FromString("http"),
	}}
	_, err := buildExternalAllowPolicy(s, nil)
	if !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("err = %v, want errNamedPortUnresolvable", err)
	}
}

func TestBuildExternalAllowPolicy_ClusterIP_WithExternalIPs(t *testing.T) {
	s := svc("admin", "ops")
	s.Spec.Type = corev1.ServiceTypeClusterIP
	s.Spec.ExternalIPs = []string{"10.0.0.1"}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol == nil {
		t.Fatal("ClusterIP+externalIPs should emit a policy")
	}
}

func TestBuildExternalAllowPolicy_ClusterIP_NoExternalIPs(t *testing.T) {
	s := svc("internal", "app")
	s.Spec.Type = corev1.ServiceTypeClusterIP
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol != nil {
		t.Errorf("plain ClusterIP shouldn't emit a policy, got: %v", pol.Name)
	}
}

func TestBuildExternalAllowPolicy_Headless(t *testing.T) {
	s := svc("headless", "app")
	s.Spec.ClusterIP = corev1.ClusterIPNone
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil || pol != nil {
		t.Errorf("headless should yield (nil, nil), got (%v, %v)", pol, err)
	}
}

func TestBuildExternalAllowPolicy_ExternalName(t *testing.T) {
	s := svc("dns-alias", "app")
	s.Spec.Type = corev1.ServiceTypeExternalName
	s.Spec.ExternalName = "elsewhere.example.com"
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil || pol != nil {
		t.Errorf("ExternalName should yield (nil, nil), got (%v, %v)", pol, err)
	}
}

func TestBuildExternalAllowPolicy_NilSelector(t *testing.T) {
	s := svc("manual-endpoints", "app")
	s.Spec.Selector = nil
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil || pol != nil {
		t.Errorf("nil-selector Service should yield (nil, nil), got (%v, %v)", pol, err)
	}
}

func TestBuildExternalAllowPolicy_MultiPort(t *testing.T) {
	s := svc("multi", "app")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)},
		{Name: "https", Port: 443, TargetPort: intstr.FromInt32(443)},
	}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := len(pol.Spec.Ingress[0].Ports); got != 2 {
		t.Errorf("expected 2 ports, got %d", got)
	}
}

func TestBuildExternalAllowPolicy_MultiPort_OneUnresolvableNamedPort(t *testing.T) {
	// Partial emission would create a confusing in-between state; one
	// unresolvable port triggers full requeue.
	s := svc("multi", "app")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)},
		{Name: "metrics", Port: 9100, TargetPort: intstr.FromString("metrics")},
	}
	_, err := buildExternalAllowPolicy(s, nil)
	if !errors.Is(err, errNamedPortUnresolvable) {
		t.Errorf("err = %v, want errNamedPortUnresolvable", err)
	}
}

func TestBuildExternalAllowPolicy_TargetPortUnset_DefaultsToPort(t *testing.T) {
	s := svc("default-target", "app")
	s.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 8080}, // TargetPort omitted
	}
	pol, err := buildExternalAllowPolicy(s, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntValue(); got != 8080 {
		t.Errorf("unset targetPort should default to Port (8080), got %d", got)
	}
}

func TestExternalAllowPolicyName_DeterministicAndCapped(t *testing.T) {
	long := "this-is-a-very-long-service-name-that-exceeds-some-limit"
	s := svc(long, "ns")
	name := externalAllowPolicyName(s)
	if len(name) > 63 {
		t.Errorf("name length %d > 63: %q", len(name), name)
	}
	if !strings.HasPrefix(name, "kube-vnet.ext.svc.") {
		t.Errorf("missing prefix: %q", name)
	}
	// Determinism.
	if name != externalAllowPolicyName(s) {
		t.Error("name is non-deterministic")
	}
}

func TestExternalAllowPolicyName_DistinctSvcsDifferentHash(t *testing.T) {
	// Two different long names that share a truncated prefix must still
	// produce distinct policy names.
	a := svc("very-long-name-aaaaaaaaaaaaaaaaaaaaaaaaaaa", "ns")
	b := svc("very-long-name-aaaaaaaaaaaaaaaaaaaaaaaaaaa-2", "ns")
	if externalAllowPolicyName(a) == externalAllowPolicyName(b) {
		t.Errorf("name collision: %q == %q", externalAllowPolicyName(a), externalAllowPolicyName(b))
	}
}

func TestExternalAllowOptedOut_AnnotationValueParsing(t *testing.T) {
	cases := map[string]bool{
		"false":   true,
		"true":    false,
		"":        false,
		"FALSE":   false, // case-sensitive on purpose: only exact "false"
		"no":      false,
		"0":       false,
		"yes":     false,
		"disable": false,
	}
	for v, wantOptOut := range cases {
		t.Run("v="+v, func(t *testing.T) {
			got := ExternalAllowOptedOut(map[string]string{AnnotationExternalAllow: v})
			if got != wantOptOut {
				t.Errorf("value %q: opted-out=%v, want %v", v, got, wantOptOut)
			}
		})
	}
	if ExternalAllowOptedOut(nil) {
		t.Error("nil annotations should not be opted-out")
	}
	if ExternalAllowOptedOut(map[string]string{}) {
		t.Error("empty annotations should not be opted-out")
	}
}

func TestIsExternallyExposed_TypeTable(t *testing.T) {
	cases := []struct {
		name string
		svc  corev1.Service
		want bool
	}{
		{"LB", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}, true},
		{"NodePort", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}}, true},
		{"ClusterIP+externalIPs", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ExternalIPs: []string{"1.2.3.4"}}}, true},
		{"ClusterIP_plain", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}, false},
		{"ExternalName", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName}}, false},
		{"Headless", corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, ClusterIP: corev1.ClusterIPNone}}, false},
		{"empty_type_with_externalIPs", corev1.Service{Spec: corev1.ServiceSpec{ExternalIPs: []string{"1.2.3.4"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isExternallyExposed(&c.svc); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestExternalAllowPolicyPredicate_FiltersBySourceKind(t *testing.T) {
	// The watch enqueues by owner reference, so this predicate is what keeps
	// the Service-source reconciler off the apiserver-reachable and host-port
	// policies.
	pred := externalAllowPolicyPredicate(LabelSourceKindService)

	cases := []struct {
		name string
		kind string
		want bool
	}{
		{name: "service_source_accepted", kind: LabelSourceKindService, want: true},
		{name: "apiserver_source_skipped", kind: LabelSourceKindApiserver, want: false},
		{name: "host_source_skipped", kind: LabelSourceKindHost, want: false},
		{name: "no_kind_label_skipped", kind: "", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns", Name: "fake",
					Labels: map[string]string{
						LabelManagedBy:  LabelManagedByValue,
						LabelRole:       LabelRoleExternalAllow,
						LabelSourceKind: c.kind,
					},
				},
			}
			if got := pred.Create(event.CreateEvent{Object: obj}); got != c.want {
				t.Errorf("predicate = %v, want %v", got, c.want)
			}
		})
	}
}

// namedPortSvc returns a Service in "ns" with a named targetPort and the
// given selector.
func namedPortSvc(name string, selector map[string]string) *corev1.Service {
	s := svc(name, "ns")
	s.Spec.Selector = selector
	s.Spec.Ports[0].TargetPort = intstr.FromString("http")
	return s
}

// podToSelectingServicesEnqueued runs fire against podToSelectingServices
// over a client holding "ns" Services web (app=web, named port), canary
// (track=canary, named port), numeric (app=web, numeric port), stamped
// (selects on a kube-vnet.system label, named port) and a Service in another
// namespace, and returns the enqueued names, sorted.
func podToSelectingServicesEnqueued(t *testing.T, fire func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request])) []string {
	t.Helper()
	numeric := svc("numeric", "ns")
	numeric.Spec.Selector = map[string]string{"app": "web"}
	other := namedPortSvc("web", map[string]string{"app": "web"})
	other.Namespace = "other"
	c := autoAllowClient(t,
		namedPortSvc("web", map[string]string{"app": "web"}),
		namedPortSvc("canary", map[string]string{"track": "canary"}),
		namedPortSvc("stamped", map[string]string{"kube-vnet.system/net.ns.payments": "both"}),
		numeric, other,
	)
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	fire(podToSelectingServices(c), q)
	var got []string
	for q.Len() > 0 {
		req, _ := q.Get()
		got = append(got, req.Namespace+"/"+req.Name)
		q.Done(req)
	}
	slices.Sort(got)
	return got
}

// A pod event reaches a Service's named-port resolution only by moving the
// pod into or out of its selector; everything else must enqueue nothing.
func TestPodToSelectingServices(t *testing.T) {
	ctx := context.Background()
	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Labels: labels}}
	}
	web := map[string]string{"app": "web"}
	cases := []struct {
		name string
		fire func(handler.Funcs, workqueue.TypedRateLimitingInterface[reconcile.Request])
		want []string
	}{
		{"create matching", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Create(ctx, event.CreateEvent{Object: pod(web)}, q)
		}, []string{"ns/web"}},
		{"create matching nothing", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Create(ctx, event.CreateEvent{Object: pod(map[string]string{"app": "db"})}, q)
		}, nil},
		{"delete matching", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Delete(ctx, event.DeleteEvent{Object: pod(map[string]string{"app": "web", "track": "canary"})}, q)
		}, []string{"ns/canary", "ns/web"}},
		{"delete matching nothing", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Delete(ctx, event.DeleteEvent{Object: pod(nil)}, q)
		}, nil},
		{"update flips in", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: pod(web), ObjectNew: pod(map[string]string{"app": "web", "track": "canary"})}, q)
		}, []string{"ns/canary"}},
		{"update flips out", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: pod(web), ObjectNew: pod(map[string]string{"app": "db"})}, q)
		}, []string{"ns/web"}},
		{"update without flip", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: pod(map[string]string{"app": "web", "v": "1"}), ObjectNew: pod(map[string]string{"app": "web", "v": "2"})}, q)
		}, nil},
		{"stamp only", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: pod(web), ObjectNew: pod(map[string]string{"app": "web", LabelSystemNetPrefix + "ns.other": "both"})}, q)
		}, nil},
		{"stamp flips a Service selecting it", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: pod(web), ObjectNew: pod(map[string]string{"app": "web", "kube-vnet.system/net.ns.payments": "both"})}, q)
		}, []string{"ns/stamped"}},
		{"status-only update", func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			running := pod(web)
			running.Status.Phase = corev1.PodRunning
			h.Update(ctx, event.UpdateEvent{ObjectOld: pod(web), ObjectNew: running}, q)
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podToSelectingServicesEnqueued(t, tc.fire); !slices.Equal(got, tc.want) {
				t.Fatalf("enqueued %v, want %v", got, tc.want)
			}
		})
	}
}

// autoAllowClient returns a fake client holding objs, with every type the
// auto-allow reconcilers read registered.
func autoAllowClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, networkingv1.AddToScheme, admissionregistrationv1.AddToScheme,
		apiregistrationv1.AddToScheme, apiextensionsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build()
}

// terminatingNamespace returns a Namespace that is being deleted.
func terminatingNamespace(name string) *corev1.Namespace {
	now := metav1.Now()
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, DeletionTimestamp: &now, Finalizers: []string{"test"},
	}}
}

// listPolicies returns the NetworkPolicies in ns.
func listPolicies(t *testing.T, c client.Client, ns string) []networkingv1.NetworkPolicy {
	t.Helper()
	var list networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list policies: %v", err)
	}
	return list.Items
}

// reconcileExternalAllow runs one reconcile of Service ns/name against objs
// and returns the client.
func reconcileExternalAllow(t *testing.T, ns, name string, objs ...client.Object) client.Client {
	t.Helper()
	c := autoAllowClient(t, objs...)
	r := &ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil), Recorder: &fakeRecorder{}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return c
}

// NamespaceLifecycle admission rejects creates in a terminating namespace, so
// applying there would only fail and retry until the namespace is gone.
func TestExternalAllowReconcile_TerminatingNamespace_NoApply(t *testing.T) {
	c := reconcileExternalAllow(t, "ns", "web", terminatingNamespace("ns"), svc("web", "ns"))
	if got := listPolicies(t, c, "ns"); len(got) != 0 {
		t.Errorf("applied %d policies into a terminating namespace", len(got))
	}
}

// Sanity check for the fake-client harness: a live namespace does get the
// policy.
func TestExternalAllowReconcile_AppliesPolicy(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
	c := reconcileExternalAllow(t, "ns", "web", ns, svc("web", "ns"))
	if got := listPolicies(t, c, "ns"); len(got) != 1 {
		t.Errorf("got %d policies, want 1", len(got))
	}
}
