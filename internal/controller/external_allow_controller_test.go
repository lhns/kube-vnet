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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/util/workqueue"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
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

func TestBuildExternalAllowPolicy_Shape(t *testing.T) {
	pol, err := buildExternalAllowPolicy(svc("traefik", "traefik"), nil)
	if err != nil || pol == nil {
		t.Fatalf("got (%v, %v), want a policy", pol, err)
	}
	for k, want := range map[string]string{
		LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow,
		LabelSource: "svc-traefik", LabelSourceKind: LabelSourceKindService,
	} {
		if pol.Labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, pol.Labels[k], want)
		}
	}
	if got := pol.Spec.PodSelector.MatchLabels["app"]; got != "traefik" {
		t.Errorf("podSelector.matchLabels[app] = %q, want traefik", got)
	}
	if len(pol.Spec.Ingress) != 1 || len(pol.Spec.Ingress[0].From) != 1 ||
		pol.Spec.Ingress[0].From[0].IPBlock == nil || pol.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Fatalf("want one from-rule with ipBlock 0.0.0.0/0, got %+v", pol.Spec.Ingress)
	}
	if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", pol.Spec.PolicyTypes)
	}
}

func TestBuildExternalAllowPolicy_Ports(t *testing.T) {
	pod := func(portName string, port int32) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Ports: []corev1.ContainerPort{{Name: portName, ContainerPort: port}},
			}}},
		}
	}
	withPorts := func(ports ...corev1.ServicePort) func(*corev1.Service) {
		return func(s *corev1.Service) { s.Spec.Ports = ports }
	}
	named := func(name string) corev1.ServicePort {
		return corev1.ServicePort{Name: name, Port: 80, TargetPort: intstr.FromString(name)}
	}
	cases := []struct {
		name           string
		mutate         func(*corev1.Service)
		pods           []corev1.Pod
		want           []int // the policy's ports; nil means no policy
		wantUnresolved bool
	}{
		{name: "load_balancer", want: []int{80}},
		// The pod-side targetPort, not the Service port or the nodePort:
		// kube-proxy DNATs to pod:targetPort before the policy applies.
		{name: "node_port_allows_target_port", mutate: func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeNodePort
			s.Spec.Ports = []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), NodePort: 32100, Protocol: corev1.ProtocolTCP}}
		}, want: []int{8080}},
		{name: "named_port_resolved_from_pod", mutate: withPorts(named("http-port")),
			pods: []corev1.Pod{pod("http-port", 8443)}, want: []int{8443}},
		// Pods may map one name to different numbers mid-rollout; the Service
		// routes to each pod's own, so each is allowed, in any listing order.
		{name: "named_port_pods_disagree", mutate: withPorts(named("http")),
			pods: []corev1.Pod{pod("http", 8080), pod("http", 9090), pod("http", 8080)}, want: []int{8080, 9090}},
		{name: "named_port_pods_disagree_reordered", mutate: withPorts(named("http")),
			pods: []corev1.Pod{pod("http", 9090), pod("http", 8080)}, want: []int{8080, 9090}},
		{name: "named_port_no_pod_yet", mutate: withPorts(named("http")), wantUnresolved: true},
		// One unresolvable port fails the whole Service rather than emitting a
		// partial policy.
		{name: "one_unresolvable_of_several", mutate: withPorts(
			corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)}, named("metrics"),
		), wantUnresolved: true},
		{name: "duplicate_target_port_once", mutate: withPorts(
			corev1.ServicePort{Name: "a", Port: 80, TargetPort: intstr.FromInt32(8080)},
			corev1.ServicePort{Name: "b", Port: 8080},
		), want: []int{8080}},
		{name: "multi_port", mutate: withPorts(
			corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)},
			corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromInt32(443)},
		), want: []int{80, 443}},
		{name: "target_port_unset_defaults_to_port", mutate: withPorts(corev1.ServicePort{Name: "http", Port: 8080}), want: []int{8080}},
		{name: "cluster_ip_with_external_ips", mutate: func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeClusterIP
			s.Spec.ExternalIPs = []string{"10.0.0.1"}
		}, want: []int{80}},
		{name: "cluster_ip_plain", mutate: func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeClusterIP }},
		{name: "headless", mutate: func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone }},
		{name: "external_name", mutate: func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeExternalName
			s.Spec.ExternalName = "elsewhere.example.com"
		}},
		{name: "no_selector", mutate: func(s *corev1.Service) { s.Spec.Selector = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := svc("api", "api")
			if c.mutate != nil {
				c.mutate(s)
			}
			pol, err := buildExternalAllowPolicy(s, c.pods)
			if c.wantUnresolved {
				if !errors.Is(err, errNamedPortUnresolvable) {
					t.Fatalf("err = %v, want errNamedPortUnresolvable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			var got []int
			if pol != nil {
				for _, p := range pol.Spec.Ingress[0].Ports {
					got = append(got, p.Port.IntValue())
				}
				if got == nil {
					got = []int{}
				}
			}
			if !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
				t.Errorf("ports = %v, want %v (nil: no policy)", got, c.want)
			}
		})
	}
}

func TestServicePolicyName(t *testing.T) {
	long := "this-is-a-very-long-service-name-that-exceeds-the-K8s-name-limit"
	for _, kind := range []string{LabelSourceKindService, LabelSourceKindApiserver} {
		name := servicePolicyName(kind, "ns", long)
		if len(name) > 63 || !strings.HasPrefix(name, "kube-vnet.ext."+kind+".this-is") {
			t.Errorf("%s: name %q is over 63 characters or misshapen", kind, name)
		}
		// The hash covers the full namespace/name, so truncation and a
		// shared name in another namespace don't collide.
		if name == servicePolicyName(kind, "ns", long+"-2") || name == servicePolicyName(kind, "other", long) {
			t.Errorf("%s: name collision for %q", kind, name)
		}
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
// (selects on a kube-vnet.system label, named port), selectorless (named port,
// no selector, so it selects no pod) and a Service in another namespace, and
// returns the enqueued names, sorted.
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
		namedPortSvc("selectorless", nil),
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
	running := pod(web)
	running.Status.Phase = corev1.PodRunning
	cases := []struct {
		name     string
		old, new *corev1.Pod // old nil: create of new; new nil: delete of old
		want     []string
	}{
		{"create matching", nil, pod(web), []string{"ns/web"}},
		{"create matching nothing", nil, pod(map[string]string{"app": "db"}), nil},
		{"delete matching", pod(map[string]string{"app": "web", "track": "canary"}), nil, []string{"ns/canary", "ns/web"}},
		{"delete matching nothing", pod(nil), nil, nil},
		{"update flips in", pod(web), pod(map[string]string{"app": "web", "track": "canary"}), []string{"ns/canary"}},
		{"update from no labels", pod(nil), pod(web), []string{"ns/web"}},
		{"update flips out", pod(web), pod(map[string]string{"app": "db"}), []string{"ns/web"}},
		{"update without flip", pod(map[string]string{"app": "web", "v": "1"}), pod(map[string]string{"app": "web", "v": "2"}), nil},
		{"stamp only", pod(web), pod(map[string]string{"app": "web", LabelSystemNetPrefix + "ns.other": "both"}), nil},
		{"stamp flips a Service selecting it", pod(web), pod(map[string]string{"app": "web", "kube-vnet.system/net.ns.payments": "both"}), []string{"ns/stamped"}},
		{"status-only update", pod(web), running, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := podToSelectingServicesEnqueued(t, func(h handler.Funcs, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				switch {
				case tc.old == nil:
					h.Create(ctx, event.CreateEvent{Object: tc.new}, q)
				case tc.new == nil:
					h.Delete(ctx, event.DeleteEvent{Object: tc.old}, q)
				default:
					h.Update(ctx, event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}, q)
				}
			})
			if !slices.Equal(got, tc.want) {
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

// Each external-allow reconciler applies its policy into a live namespace but
// not into a terminating one: NamespaceLifecycle admission would reject the
// create, so it would only fail and retry until the namespace is gone.
func TestExternalAllowReconcilers_TerminatingNamespace_NoApply(t *testing.T) {
	cases := []struct {
		name      string
		obj       client.Object
		req       string // namespace/name, or the namespace alone
		reconcile func(client.Client) reconcileFunc
	}{
		{"external_allow", svc("web", "ns"), "ns/web", func(c client.Client) reconcileFunc {
			return (&ExternalAllowReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil)}).Reconcile
		}},
		{"apiserver_reachable", optedInService("ns", "webhook"), "ns/webhook", func(c client.Client) reconcileFunc {
			return (&ApiserverReachableReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil)}).Reconcile
		}},
		{"host_port", podWithHostPorts("p", corev1.ContainerPort{HostPort: 8080, Protocol: corev1.ProtocolTCP}), "ns",
			func(c client.Client) reconcileFunc {
				return (&HostPortReconciler{Client: c, Scheme: c.Scheme(), NSFilter: NewNamespaceFilter(nil)}).Reconcile
			}},
	}
	for _, tc := range cases {
		for ns, want := range map[*corev1.Namespace]int{mkNamespace("ns", nil): 1, terminatingNamespace("ns"): 0} {
			c := autoAllowClient(t, ns, tc.obj.DeepCopyObject().(client.Object))
			reqNS, reqName, ok := strings.Cut(tc.req, "/")
			if !ok {
				reqNS, reqName = "", reqNS
			}
			if err := reconcileName(t, tc.reconcile(c), reqNS, reqName); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := len(listPolicies(t, c, "ns")); got != want {
				t.Errorf("%s, terminating=%v: %d policies, want %d", tc.name, ns.DeletionTimestamp != nil, got, want)
			}
		}
	}
}
