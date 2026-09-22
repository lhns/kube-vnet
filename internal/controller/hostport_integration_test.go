//go:build integration

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// makeHostPortPod creates a pod with one container declaring (hostPort, protocol).
func makeHostPortPod(ns, name string, hostPort int32, protocol corev1.Protocol) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "nginx",
				Ports: []corev1.ContainerPort{{HostPort: hostPort, ContainerPort: 80, Protocol: protocol}},
			}},
		},
	}
}

func TestIntegration_HostPort_PodCreated_PolicyAppears(t *testing.T) {
	ns := uniqueNS(t, "hp-create")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeHostPortPod(ns, "web", 18080, corev1.ProtocolTCP))

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18080, protocol: corev1.ProtocolTCP})
	waitForPolicy(t, ns, wantName, 10*time.Second)
}

func TestIntegration_HostPort_TwoPodsSamePort_OneIdempotentPolicy(t *testing.T) {
	ns := uniqueNS(t, "hp-idem")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeHostPortPod(ns, "web-a", 18081, corev1.ProtocolTCP))
	mustCreate(t, makeHostPortPod(ns, "web-b", 18081, corev1.ProtocolTCP))

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18081, protocol: corev1.ProtocolTCP})
	waitForPolicy(t, ns, wantName, 10*time.Second)
	// Verify only ONE host-source policy in this NS for this (port, proto).
	time.Sleep(2 * time.Second)
	var all networkingv1.NetworkPolicyList
	if err := testClient.List(context.Background(), &all,
		client.InNamespace(ns),
		client.MatchingLabels{LabelManagedBy: LabelManagedByValue, LabelRole: LabelRoleExternalAllow},
	); err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for i := range all.Items {
		l := all.Items[i].Labels
		if l[LabelSourceKind] == LabelSourceKindHost && l[LabelSource] == "host-18081-tcp" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 host-source policy for 18081/tcp, got %d", count)
	}
}

func TestIntegration_HostPort_ProtocolDistinct(t *testing.T) {
	ns := uniqueNS(t, "hp-proto")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeHostPortPod(ns, "tcp-pod", 18082, corev1.ProtocolTCP))
	mustCreate(t, makeHostPortPod(ns, "udp-pod", 18082, corev1.ProtocolUDP))

	tcpName := hostPortPolicyName(ns, hostPortKey{port: 18082, protocol: corev1.ProtocolTCP})
	udpName := hostPortPolicyName(ns, hostPortKey{port: 18082, protocol: corev1.ProtocolUDP})
	waitForPolicy(t, ns, tcpName, 10*time.Second)
	waitForPolicy(t, ns, udpName, 10*time.Second)
}

func TestIntegration_HostPort_PodDeleted_PolicyRetainedUntilLast(t *testing.T) {
	ns := uniqueNS(t, "hp-retain")
	mustCreate(t, makeNamespace(ns, nil, nil))
	a := makeHostPortPod(ns, "a", 18083, corev1.ProtocolTCP)
	b := makeHostPortPod(ns, "b", 18083, corev1.ProtocolTCP)
	mustCreate(t, a)
	mustCreate(t, b)

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18083, protocol: corev1.ProtocolTCP})
	waitForPolicy(t, ns, wantName, 10*time.Second)

	// Delete a. Policy should remain (b still exposes the port).
	if err := testClient.Delete(context.Background(), a); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	time.Sleep(3 * time.Second)
	if _, err := findPolicy(context.Background(), ns, wantName); err != nil {
		t.Errorf("policy should still exist after deleting one of two pods, got err=%v", err)
	}

	// Delete b. Policy should now be collected.
	if err := testClient.Delete(context.Background(), b); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	waitForPolicyAbsent(t, ns, wantName, 10*time.Second)
}

func TestIntegration_HostPort_DisabledNS_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "hp-disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, makeHostPortPod(ns, "web", 18084, corev1.ProtocolTCP))

	assertPolicyStaysAbsent(t, ns, hostPortPolicyName(ns, hostPortKey{port: 18084, protocol: corev1.ProtocolTCP}), 3*time.Second)
}

func TestIntegration_HostPort_NSAnnotationOptOut(t *testing.T) {
	ns := uniqueNS(t, "hp-opt-out")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeHostPortPod(ns, "web", 18085, corev1.ProtocolTCP))

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18085, protocol: corev1.ProtocolTCP})
	waitForPolicy(t, ns, wantName, 10*time.Second)

	updateNamespace(t, ns, func(n *corev1.Namespace) {
		metav1.SetMetaDataAnnotation(&n.ObjectMeta, AnnotationExternalAllow, "false")
	})

	waitForPolicyAbsent(t, ns, wantName, 10*time.Second)
}

func TestIntegration_HostPort_StampApplied(t *testing.T) {
	ns := uniqueNS(t, "hp-stamp")
	mustCreate(t, makeNamespace(ns, nil, nil))
	pod := makeHostPortPod(ns, "web", 18086, corev1.ProtocolTCP)
	mustCreate(t, pod)

	wantStamp := LabelSystemHostPortPrefix + "18086.tcp"
	eventually(t, 10*time.Second, func() error {
		var p corev1.Pod
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &p); err != nil {
			return err
		}
		if p.Labels[wantStamp] != "true" {
			return fmt.Errorf("stamp not yet applied")
		}
		return nil
	})
}

func TestIntegration_HostPort_HostNetworkPod_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "hp-hostnet")
	mustCreate(t, makeNamespace(ns, nil, nil))
	pod := makeHostPortPod(ns, "web", 18087, corev1.ProtocolTCP)
	// hostNetwork=true requires hostPort==containerPort per K8s validation.
	pod.Spec.Containers[0].Ports[0].ContainerPort = 18087
	pod.Spec.HostNetwork = true
	mustCreate(t, pod)

	assertPolicyStaysAbsent(t, ns, hostPortPolicyName(ns, hostPortKey{port: 18087, protocol: corev1.ProtocolTCP}), 3*time.Second)
}
