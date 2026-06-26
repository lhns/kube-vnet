package controller

import (
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

func TestDesiredHostPortKeys_SinglePodSinglePort(t *testing.T) {
	p := podWithHostPorts("p1", corev1.ContainerPort{HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolTCP})
	got := desiredHostPortKeys([]corev1.Pod{*p})
	if len(got) != 1 {
		t.Fatalf("expected 1 key, got %d", len(got))
	}
	if !got[hostPortKey{port: 8080, protocol: corev1.ProtocolTCP}] {
		t.Errorf("missing expected key 8080/TCP, got: %v", got)
	}
}

func TestDesiredHostPortKeys_SamePortDifferentProtocol(t *testing.T) {
	a := podWithHostPorts("a", corev1.ContainerPort{HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolTCP})
	b := podWithHostPorts("b", corev1.ContainerPort{HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolUDP})
	got := desiredHostPortKeys([]corev1.Pod{*a, *b})
	if len(got) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(got))
	}
	if !got[hostPortKey{port: 8080, protocol: corev1.ProtocolTCP}] {
		t.Error("missing 8080/TCP")
	}
	if !got[hostPortKey{port: 8080, protocol: corev1.ProtocolUDP}] {
		t.Error("missing 8080/UDP")
	}
}

func TestDesiredHostPortKeys_MultiContainerMultiPort(t *testing.T) {
	p := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "web", Ports: []corev1.ContainerPort{{HostPort: 80, Protocol: corev1.ProtocolTCP}}},
				{Name: "tls", Ports: []corev1.ContainerPort{{HostPort: 443, Protocol: corev1.ProtocolTCP}}},
			},
		},
	}
	got := desiredHostPortKeys([]corev1.Pod{*p})
	if len(got) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(got))
	}
}

func TestDesiredHostPortKeys_NoHostPort_Skipped(t *testing.T) {
	p := podWithHostPorts("p", corev1.ContainerPort{ContainerPort: 80, Protocol: corev1.ProtocolTCP})
	got := desiredHostPortKeys([]corev1.Pod{*p})
	if len(got) != 0 {
		t.Errorf("expected 0 keys (no hostPort declared), got %d: %v", len(got), got)
	}
}

func TestDesiredHostPortKeys_HostNetworkPod_Skipped(t *testing.T) {
	// hostNetwork pods are out of scope per ADR 0040 — NetworkPolicy
	// enforcement is CNI-dependent.
	p := podWithHostPorts("p", corev1.ContainerPort{HostPort: 8080, Protocol: corev1.ProtocolTCP})
	p.Spec.HostNetwork = true
	got := desiredHostPortKeys([]corev1.Pod{*p})
	if len(got) != 0 {
		t.Errorf("hostNetwork pod should be skipped, got %d keys: %v", len(got), got)
	}
}

func TestDesiredHostPortKeys_DefaultProtocolIsTCP(t *testing.T) {
	// container.ports[].protocol is optional; default is TCP per K8s schema.
	p := podWithHostPorts("p", corev1.ContainerPort{HostPort: 9999}) // no protocol set
	got := desiredHostPortKeys([]corev1.Pod{*p})
	if !got[hostPortKey{port: 9999, protocol: corev1.ProtocolTCP}] {
		t.Errorf("default protocol should be TCP, got: %v", got)
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

func TestHostPortPolicyName_Format(t *testing.T) {
	name := hostPortPolicyName("traefik", hostPortKey{port: 80, protocol: corev1.ProtocolTCP})
	if !strings.HasPrefix(name, "kube-vnet.ext.host.80.tcp-") {
		t.Errorf("name = %q, want prefix kube-vnet.ext.host.80.tcp-", name)
	}
	if len(name) > 63 {
		t.Errorf("name length %d > 63: %q", len(name), name)
	}
}

func TestHostPortPolicyName_NSInHash(t *testing.T) {
	// Same (port, proto) in different NSes → different policy names.
	a := hostPortPolicyName("nsA", hostPortKey{port: 80, protocol: corev1.ProtocolTCP})
	b := hostPortPolicyName("nsB", hostPortKey{port: 80, protocol: corev1.ProtocolTCP})
	if a == b {
		t.Errorf("expected different names for different NSes, both = %q", a)
	}
}

func TestParseHostSourceLabel_RoundTrip(t *testing.T) {
	cases := []hostPortKey{
		{port: 80, protocol: corev1.ProtocolTCP},
		{port: 8080, protocol: corev1.ProtocolUDP},
		{port: 5000, protocol: corev1.ProtocolSCTP},
	}
	for _, c := range cases {
		val := "host-" + (hostPortKey{port: c.port, protocol: c.protocol}).String()
		// Convert to the LabelSource value as buildHostPortPolicy does: <port>-<proto-lower>
		// (vs hostPortKey.String() which uses dot separator).
		// Use the exact format the builder emits:
		valBuilder := buildHostPortPolicy("ns", c).Labels[LabelSource]
		got, ok := parseHostSourceLabel(valBuilder)
		if !ok {
			t.Errorf("parse failed for %q (built from %v); raw=%q", valBuilder, c, val)
			continue
		}
		if got != c {
			t.Errorf("parsed %v, want %v (source=%q)", got, c, valBuilder)
		}
	}
}

func TestParseHostSourceLabel_RejectsServiceSource(t *testing.T) {
	// Service-source LabelSource values are bare service names, no "host-" prefix.
	_, ok := parseHostSourceLabel("traefik")
	if ok {
		t.Error("expected parse to reject service-source label")
	}
}

func TestDesiredHostPortStamps_RespectsHostNetwork(t *testing.T) {
	p := podWithHostPorts("p", corev1.ContainerPort{HostPort: 8080, Protocol: corev1.ProtocolTCP})
	stamps := desiredHostPortStamps(p)
	if len(stamps) != 1 {
		t.Errorf("expected 1 stamp, got %d", len(stamps))
	}
	p.Spec.HostNetwork = true
	stamps = desiredHostPortStamps(p)
	if len(stamps) != 0 {
		t.Errorf("hostNetwork pod should have no stamps, got %d", len(stamps))
	}
}

func TestDesiredHostPortStamps_LabelFormat(t *testing.T) {
	p := podWithHostPorts("p",
		corev1.ContainerPort{HostPort: 80, Protocol: corev1.ProtocolTCP},
		corev1.ContainerPort{HostPort: 9999, Protocol: corev1.ProtocolUDP},
	)
	stamps := desiredHostPortStamps(p)
	want1 := LabelSystemHostPortPrefix + "80.tcp"
	want2 := LabelSystemHostPortPrefix + "9999.udp"
	if !stamps[want1] {
		t.Errorf("missing %q", want1)
	}
	if !stamps[want2] {
		t.Errorf("missing %q", want2)
	}
}
