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
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart manifest validation test")
	}

	chartDir := filepath.Join("..", "..", "charts", "kube-vnet")

	cases := []struct {
		name string
		sets []string
	}{
		// All three isolation level presets, plus an explicit-memberships
		// override case. operator.clusterBaseline.ingressIsolationLevel has
		// no default per ADR 0031 — set it explicitly for every case.
		{"isolation-pod", []string{
			"--set", "operator.clusterBaseline.ingressIsolationLevel=pod",
		}},
		{"isolation-namespace", []string{
			"--set", "operator.clusterBaseline.ingressIsolationLevel=namespace",
		}},
		{"isolation-cluster", []string{
			"--set", "operator.clusterBaseline.ingressIsolationLevel=cluster",
		}},
		// podMonitor.enabled=true is intentionally left out: it renders a
		// PodMonitor (prometheus-operator CRD) the envtest apiserver doesn't
		// know about — a real cluster would have it from the operator install.
		// metricsService is a plain Service, fine to validate.
		{"with-metrics-svc-and-explicit-memberships", []string{
			"--set", "metricsService.enabled=true",
			"--set", "operator.clusterBaseline.memberships.namespace=default-both",
			"--set", "operator.clusterBaseline.memberships.cluster=default-egress",
		}},
	}
	for _, tc := range cases {
		t.Run("chart-"+tc.name, func(t *testing.T) {
			args := append([]string{
				"template", "testrelease", chartDir,
				"--kube-version", "1.31.0",
			}, tc.sets...)
			var stdout, stderr bytes.Buffer
			cmd := exec.Command("helm", args...)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("helm template: %v\nstderr: %s", err, stderr.String())
			}
			applyAllDryRun(t, &stdout)
		})
	}

	// Also exercise each kustomize-shipped VAP directly. The direction-VAP
	// is hand-written in config/admission/policy.yaml; the system-labels and
	// system-vnet VAPs are generated from the chart templates by
	// `make render-kustomize-vaps`. Reading them as static files avoids
	// needing kubectl on PATH in the integration job.
	for _, p := range []string{"policy.yaml", "system-labels-vap.yaml", "system-vnet-vap.yaml"} {
		p := p
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

// applyAllDryRun decodes every YAML document in `in` and applies it with
// DryRunAll, surfacing any per-document apiserver rejection as a t.Errorf.
// It skips CRDs (already installed by TestMain) and empty separator docs.
func applyAllDryRun(t *testing.T, in io.Reader) {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(in, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("decode YAML: %v", err)
		}
		if obj.Object == nil || obj.GetKind() == "" {
			continue
		}
		if obj.GetKind() == "CustomResourceDefinition" {
			continue
		}
		if err := testClient.Create(context.Background(), obj, client.DryRunAll); err != nil {
			t.Errorf("dry-run create %s %s/%s: %v",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
}
