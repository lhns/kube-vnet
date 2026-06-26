//go:build integration

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		return testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
	})
}

func TestIntegration_HostPort_TwoPodsSamePort_OneIdempotentPolicy(t *testing.T) {
	ns := uniqueNS(t, "hp-idem")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeHostPortPod(ns, "web-a", 18081, corev1.ProtocolTCP))
	mustCreate(t, makeHostPortPod(ns, "web-b", 18081, corev1.ProtocolTCP))

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18081, protocol: corev1.ProtocolTCP})
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		return testClient.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
	})
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
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: tcpName}, &pol); err != nil {
			return err
		}
		return testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: udpName}, &pol)
	})
}

func TestIntegration_HostPort_PodDeleted_PolicyRetainedUntilLast(t *testing.T) {
	ns := uniqueNS(t, "hp-retain")
	mustCreate(t, makeNamespace(ns, nil, nil))
	a := makeHostPortPod(ns, "a", 18083, corev1.ProtocolTCP)
	b := makeHostPortPod(ns, "b", 18083, corev1.ProtocolTCP)
	mustCreate(t, a)
	mustCreate(t, b)

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18083, protocol: corev1.ProtocolTCP})
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		return testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
	})

	// Delete a. Policy should remain (b still exposes the port).
	if err := testClient.Delete(context.Background(), a); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	time.Sleep(3 * time.Second)
	var pol networkingv1.NetworkPolicy
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: wantName}, &pol); err != nil {
		t.Errorf("policy should still exist after deleting one of two pods, got err=%v", err)
	}

	// Delete b. Policy should now be collected.
	if err := testClient.Delete(context.Background(), b); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	eventually(t, 10*time.Second, func() error {
		err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return &simpleErr{"policy still exists"}
	})
}

func TestIntegration_HostPort_DisabledNS_NoEmission(t *testing.T) {
	ns := uniqueNS(t, "hp-disabled")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))
	mustCreate(t, makeHostPortPod(ns, "web", 18084, corev1.ProtocolTCP))

	time.Sleep(3 * time.Second)
	wantName := hostPortPolicyName(ns, hostPortKey{port: 18084, protocol: corev1.ProtocolTCP})
	var pol networkingv1.NetworkPolicy
	err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no policy in disabled NS, got err=%v", err)
	}
}

func TestIntegration_HostPort_NSAnnotationOptOut(t *testing.T) {
	ns := uniqueNS(t, "hp-opt-out")
	mustCreate(t, makeNamespace(ns, nil, nil))
	mustCreate(t, makeHostPortPod(ns, "web", 18085, corev1.ProtocolTCP))

	wantName := hostPortPolicyName(ns, hostPortKey{port: 18085, protocol: corev1.ProtocolTCP})
	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		return testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
	})

	// Annotate NS opt-out.
	eventually(t, 5*time.Second, func() error {
		var nsObj corev1.Namespace
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: ns}, &nsObj); err != nil {
			return err
		}
		if nsObj.Annotations == nil {
			nsObj.Annotations = map[string]string{}
		}
		nsObj.Annotations[AnnotationExternalAllow] = "false"
		return testClient.Update(context.Background(), &nsObj)
	})

	eventually(t, 10*time.Second, func() error {
		var pol networkingv1.NetworkPolicy
		err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return &simpleErr{"policy still exists"}
	})
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
			return &simpleErr{"stamp not yet applied"}
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

	time.Sleep(3 * time.Second)
	wantName := hostPortPolicyName(ns, hostPortKey{port: 18087, protocol: corev1.ProtocolTCP})
	var pol networkingv1.NetworkPolicy
	err := testClient.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: wantName}, &pol)
	if !apierrors.IsNotFound(err) {
		t.Errorf("hostNetwork pod should not trigger host-port policy, got err=%v", err)
	}
}
