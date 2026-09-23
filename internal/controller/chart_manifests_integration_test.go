//go:build integration

package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegration_ChartManifestsValidAgainstAPIServer renders the Helm chart
// in the configurations the helm CI job already smoke-tests and applies each
// rendered manifest with DryRunAll against envtest's apiserver. Catches CEL
// compilation errors in ValidatingAdmissionPolicy, OpenAPI schema mistakes
// in CRDs, and any other class of error the apiserver rejects at admission.
// See ADR 0027.
func TestIntegration_ChartManifestsValidAgainstAPIServer(t *testing.T) {
	cases := []struct {
		name string
		sets []string
	}{
		// operator.clusterBaseline.ingressIsolationLevel has no default (ADR
		// 0031), so every case sets it.
		{"isolation-pod", []string{
			"--set", "operator.clusterBaseline.ingressIsolationLevel=pod",
		}},
		{"isolation-namespace", []string{
			"--set", "operator.clusterBaseline.ingressIsolationLevel=namespace",
		}},
		{"isolation-cluster", []string{
			"--set", "operator.clusterBaseline.ingressIsolationLevel=cluster",
		}},
		// podMonitor.enabled=true is left out: it renders a PodMonitor, a
		// prometheus-operator CRD that envtest doesn't have.
		{"with-metrics-svc-and-explicit-memberships", []string{
			"--set", "metricsService.enabled=true",
			"--set", "operator.clusterBaseline.memberships.namespace=default-both",
			"--set", "operator.clusterBaseline.memberships.cluster=default-egress",
		}},
	}
	for _, tc := range cases {
		t.Run("chart-"+tc.name, func(t *testing.T) {
			applyAllDryRun(t, bytes.NewReader(helmTemplate(t, "testrelease", tc.sets...)))
		})
	}

	// The kustomize-shipped VAPs are rendered from the chart templates by
	// `make render-kustomize-vaps`; read them as static files so this needs
	// no kubectl.
	for _, p := range []string{"validating-admission-policy.yaml", "system-labels-vap.yaml", "system-vnet-vap.yaml"} {
		t.Run("kustomize-vap-"+p, func(t *testing.T) {
			path := filepath.Join("..", "..", "config", "admission", p)
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			defer f.Close()
			applyAllDryRun(t, f)
		})
	}
}

// applyAllDryRun applies every document in `in` with DryRunAll, reporting each
// rejection. CRDs are skipped: TestMain already installed them.
func applyAllDryRun(t *testing.T, in io.Reader) {
	t.Helper()
	for _, obj := range decodeObjects(t, in) {
		if obj.GetKind() == "CustomResourceDefinition" {
			continue
		}
		if err := testClient.Create(context.Background(), obj, client.DryRunAll); err != nil {
			t.Errorf("dry-run create %s %s/%s: %v",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
}

// helmTemplate runs `helm template <release>` on the chart with extra args
// and returns the rendered YAML. Skips the test when helm is not on PATH.
func helmTemplate(t *testing.T, release string, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	full := append([]string{
		"template", release, filepath.Join("..", "..", "charts", "kube-vnet"),
		"--kube-version", "1.31.0",
	}, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("helm", full...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.Bytes()
}

// decodeObjects decodes every document of a YAML stream, dropping empty ones.
func decodeObjects(t *testing.T, in io.Reader) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(in, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("decode YAML: %v", err)
		}
		if obj.Object == nil || obj.GetKind() == "" {
			continue
		}
		out = append(out, obj)
	}
}
