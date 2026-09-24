package controller

import (
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWithHostPorts(name string, ports ...corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx", Ports: ports}},
		},
	}
}

func TestDesiredHostPortKeys(t *testing.T) {
	tcp := func(port int32) corev1.ContainerPort {
		return corev1.ContainerPort{HostPort: port, ContainerPort: 80, Protocol: corev1.ProtocolTCP}
	}
	key := func(port int32, proto corev1.Protocol) hostPortKey { return hostPortKey{port: port, protocol: proto} }
	hostNet := podWithHostPorts("p", tcp(8080))
	hostNet.Spec.HostNetwork = true
	cases := []struct {
		name string
		pods []*corev1.Pod
		want map[hostPortKey]bool
	}{
		{"single_port", []*corev1.Pod{podWithHostPorts("p", tcp(8080))}, map[hostPortKey]bool{key(8080, corev1.ProtocolTCP): true}},
		{"same_port_both_protocols", []*corev1.Pod{
			podWithHostPorts("a", tcp(8080)),
			podWithHostPorts("b", corev1.ContainerPort{HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolUDP}),
		}, map[hostPortKey]bool{key(8080, corev1.ProtocolTCP): true, key(8080, corev1.ProtocolUDP): true}},
		{"several_ports", []*corev1.Pod{podWithHostPorts("p", tcp(80), tcp(443))},
			map[hostPortKey]bool{key(80, corev1.ProtocolTCP): true, key(443, corev1.ProtocolTCP): true}},
		{"no_host_port", []*corev1.Pod{podWithHostPorts("p", corev1.ContainerPort{ContainerPort: 80})}, map[hostPortKey]bool{}},
		// Out of scope (ADR 0040): NetworkPolicy on hostNetwork pods is
		// CNI-dependent.
		{"host_network_skipped", []*corev1.Pod{hostNet}, map[hostPortKey]bool{}},
		{"protocol_defaults_to_tcp", []*corev1.Pod{podWithHostPorts("p", corev1.ContainerPort{HostPort: 9999})},
			map[hostPortKey]bool{key(9999, corev1.ProtocolTCP): true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var pods []corev1.Pod
			for _, p := range c.pods {
				pods = append(pods, *p)
			}
			if got := desiredHostPortKeys(pods); !maps.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestBuildHostPortPolicy_ShapeAndLabels(t *testing.T) {
	pol := buildHostPortPolicy("ns1", hostPortKey{port: 8080, protocol: corev1.ProtocolTCP})
	if pol.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("missing managed-by label")
	}
	if pol.Labels[LabelRole] != LabelRoleExternalAllow {
		t.Errorf("wrong role: %q", pol.Labels[LabelRole])
	}
	if pol.Labels[LabelSource] != "host-8080-tcp" {
		t.Errorf("wrong source: %q", pol.Labels[LabelSource])
	}
	if pol.Labels[LabelSourceKind] != LabelSourceKindHost {
		t.Errorf("wrong source-kind: %q", pol.Labels[LabelSourceKind])
	}
	// podSelector matches the host-port stamp for this (port, proto).
	want := LabelSystemHostPortPrefix + "8080.tcp"
	if pol.Spec.PodSelector.MatchLabels[want] != "true" {
		t.Errorf("podSelector missing %q=true, got: %v", want, pol.Spec.PodSelector.MatchLabels)
	}
	// Allow on the right port + protocol.
	port := pol.Spec.Ingress[0].Ports[0]
	if port.Port.IntValue() != 8080 || *port.Protocol != corev1.ProtocolTCP {
		t.Errorf("wrong allow port: %+v", port)
	}
	// ipBlock 0.0.0.0/0.
	if pol.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("wrong ipBlock: %q", pol.Spec.Ingress[0].From[0].IPBlock.CIDR)
	}
}

func TestHostPortPolicyName(t *testing.T) {
	key := hostPortKey{port: 80, protocol: corev1.ProtocolTCP}
	name := hostPortPolicyName("traefik", key)
	if !strings.HasPrefix(name, "kube-vnet.ext.host.80.tcp-") || len(name) > 63 {
		t.Errorf("name = %q, want prefix kube-vnet.ext.host.80.tcp- and at most 63 characters", name)
	}
	if name == hostPortPolicyName("other", key) {
		t.Errorf("the namespace must be in the hash, both = %q", name)
	}
}

func TestDesiredHostPortStamps(t *testing.T) {
	p := podWithHostPorts("p",
		corev1.ContainerPort{HostPort: 80, Protocol: corev1.ProtocolTCP},
		corev1.ContainerPort{HostPort: 9999, Protocol: corev1.ProtocolUDP},
	)
	want := map[string]bool{LabelSystemHostPortPrefix + "80.tcp": true, LabelSystemHostPortPrefix + "9999.udp": true}
	if got := desiredHostPortStamps(p); !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	p.Spec.HostNetwork = true
	if got := desiredHostPortStamps(p); len(got) != 0 {
		t.Errorf("hostNetwork pod should have no stamps, got %v", got)
	}
}
