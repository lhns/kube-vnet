//go:build integration

package podresolution

import (
	"context"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testNetworkWaitImage = "ghcr.io/lhns/kube-vnet:it"

// warningRecorder collects the admission warnings the apiserver returns.
type warningRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (w *warningRecorder) HandleWarningHeaderWithContext(_ context.Context, _ int, _ string, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = append(w.messages, message)
}

// The wait container is part of the pod the apiserver stores, ahead of the
// pod's own init containers: it must run before anything else in the pod.
func TestIntegration_Webhook_NetworkWaitInjectedFirst(t *testing.T) {
	ns := webhookNS(t, "web")

	pod := makePod(ns, "waits", map[string]string{"kube-vnet/net.web": "both"})
	pod.Annotations = map[string]string{AnnotationNetworkMaxWait: "20s"}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, pod.Spec.Containers[0])
	pod.Spec.InitContainers[0].Name = "own-init"
	mustCreate(t, pod)

	inits := pod.Spec.InitContainers
	if len(inits) != 2 || inits[0].Name != NetworkWaitContainerName || inits[1].Name != "own-init" {
		t.Fatalf("init containers in the create response: %v, want [%s own-init]", names(inits), NetworkWaitContainerName)
	}
	if inits[0].Image != testNetworkWaitImage {
		t.Errorf("wait image %q, want %q", inits[0].Image, testNetworkWaitImage)
	}
	if !strings.Contains(strings.Join(inits[0].Args, " "), "--max-wait=20s") {
		t.Errorf("wait args %v lack the pod's max wait", inits[0].Args)
	}
}

// An opt-in that can't be honoured reaches the user as a kubectl warning, and
// the pod is still admitted.
func TestIntegration_Webhook_NetworkWaitInvalidValueWarns(t *testing.T) {
	ns := webhookNS(t, "web")

	rec := &warningRecorder{}
	cfg := rest.CopyConfig(testCfg)
	cfg.WarningHandlerWithContext = rec
	cl, err := client.New(cfg, client.Options{Scheme: itScheme})
	if err != nil {
		t.Fatal(err)
	}

	pod := makePod(ns, "bad-wait", map[string]string{"kube-vnet/net.web": "both"})
	pod.Annotations = map[string]string{AnnotationNetworkMaxWait: "soon"}
	if err := cl.Create(context.Background(), pod); err != nil {
		t.Fatalf("pod with an invalid max wait was rejected: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), pod) })

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("wait injected despite an invalid max wait: %v", names(pod.Spec.InitContainers))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !strings.Contains(strings.Join(rec.messages, "\n"), `"soon" is not a positive duration`) {
		t.Errorf("warnings %q do not report the invalid value", rec.messages)
	}
}
