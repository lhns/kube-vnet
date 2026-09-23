package podresolution

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var testNetworkWait = &NetworkWaitConfig{
	Image:   "ghcr.io/lhns/kube-vnet:test",
	Beacons: "kube-vnet-network-beacon.kube-vnet-system.svc:9444",
}

// withMaxWait is a member of vnet web in namespace app, opting into the wait.
func withMaxWait(value string) *corev1.Pod {
	p := pod("app", map[string]string{"kube-vnet/net.web": "both"})
	p.Annotations = map[string]string{AnnotationNetworkMaxWait: value}
	return p
}

func TestNetworkWait_Injection(t *testing.T) {
	userInit := corev1.Container{Name: "migrate", Image: "migrate:1"}
	alreadyInjected := withMaxWait("30s")
	alreadyInjected.Spec.InitContainers = []corev1.Container{{Name: NetworkWaitContainerName}, userInit}
	hostNet := withMaxWait("30s")
	hostNet.Spec.HostNetwork = true
	withUserInit := withMaxWait("30s")
	withUserInit.Spec.InitContainers = []corev1.Container{userInit}

	for _, tc := range []struct {
		name        string
		pod         *corev1.Pod
		oldPod      *corev1.Pod // non-nil makes it an UPDATE
		cfg         *NetworkWaitConfig
		disabled    []string // unmanaged namespaces
		wantInits   []string // init container names, in order
		wantWarning string   // substring; "" means no warning
	}{
		{name: "annotation absent", pod: pod("app", map[string]string{"kube-vnet/net.web": "both"}),
			cfg: testNetworkWait},
		{name: "valid max wait", pod: withMaxWait("30s"), cfg: testNetworkWait,
			wantInits: []string{NetworkWaitContainerName}},
		{name: "runs before the pod's own init containers", pod: withUserInit, cfg: testNetworkWait,
			wantInits: []string{NetworkWaitContainerName, "migrate"}},
		{name: "not a duration", pod: withMaxWait("soon"), cfg: testNetworkWait,
			wantWarning: `"soon" is not a positive duration`},
		{name: "zero duration", pod: withMaxWait("0s"), cfg: testNetworkWait,
			wantWarning: `"0s" is not a positive duration`},
		{name: "feature disabled", pod: withMaxWait("30s"), cfg: nil,
			wantWarning: "webhook.networkWait.enabled"},
		{name: "update", pod: withMaxWait("30s"), oldPod: withMaxWait("30s"), cfg: testNetworkWait},
		{name: "hostNetwork", pod: hostNet, cfg: testNetworkWait},
		{name: "unmanaged namespace", pod: withMaxWait("30s"), cfg: testNetworkWait, disabled: []string{"app"}},
		{name: "already injected", pod: alreadyInjected, cfg: testNetworkWait,
			wantInits: []string{NetworkWaitContainerName, "migrate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, []client.Object{vnet("web", "app", nil)}, tc.pod)
			d := newDeps(t, c, tc.disabled...)
			d.NetworkWait = tc.cfg

			out, resp := mutateWithResponse(t, &Mutator{d}, "alice", tc.oldPod, tc.pod)

			var got []string
			for _, ic := range out.Spec.InitContainers {
				got = append(got, ic.Name)
			}
			if !reflect.DeepEqual(got, tc.wantInits) {
				t.Errorf("init containers %v, want %v", got, tc.wantInits)
			}
			warnings := strings.Join(resp.Warnings, "\n")
			if tc.wantWarning == "" && warnings != "" {
				t.Errorf("unexpected warning: %s", warnings)
			}
			if tc.wantWarning != "" && !strings.Contains(warnings, tc.wantWarning) {
				t.Errorf("warning %q does not mention %q", warnings, tc.wantWarning)
			}
		})
	}
}

// The injected container runs the operator's network-wait subcommand with the
// pod's own maximum, and passes PodSecurity "restricted".
func TestNetworkWait_ContainerSpec(t *testing.T) {
	p := withMaxWait("45s")
	c := newClient(t, []client.Object{vnet("web", "app", nil)}, p)
	d := newDeps(t, c)
	d.NetworkWait = testNetworkWait

	out := mutate(t, &Mutator{d}, "alice", nil, p)
	wait := out.Spec.InitContainers[0]

	if wait.Image != testNetworkWait.Image || wait.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("image %q (%s), want %q (IfNotPresent)", wait.Image, wait.ImagePullPolicy, testNetworkWait.Image)
	}
	wantArgs := []string{"network-wait", "--beacons=" + testNetworkWait.Beacons, "--max-wait=45s"}
	if !reflect.DeepEqual(wait.Args, wantArgs) {
		t.Errorf("args %v, want %v", wait.Args, wantArgs)
	}
	sc := wait.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("security context is not PodSecurity-restricted: %+v", sc)
	}
	if wait.Resources.Limits.Memory().IsZero() || wait.Resources.Requests.Cpu().IsZero() {
		t.Errorf("requests and limits must be set (namespaces with ResourceQuota): %+v", wait.Resources)
	}
	// Stamping is unaffected by the injection.
	if out.Labels["kube-vnet.system/net.app.web"] != "both" {
		t.Errorf("stamp missing next to the injection: %v", out.Labels)
	}
}
