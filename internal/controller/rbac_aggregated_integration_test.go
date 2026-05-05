//go:build integration

package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// End-user RBAC tests for the chart-shipped aggregated ClusterRoles
// (ADR 0031 cleanup). The chart emits editor + viewer ClusterRoles for each
// CRD; the namespace-scoped ones aggregate into upstream `admin`/`edit`/
// `view`. This file installs the ClusterRoles directly (no aggregation
// dependency on envtest) and binds an impersonated user to each via either
// a RoleBinding (NS-scoped editor) or a ClusterRoleBinding (cluster
// editor).

// chartReleaseName must match the helm release name used for rendering. The
// rbac-aggregated.yaml template names roles `<release>-<chartname>-<resource>-<role>`.
const chartReleaseName = "rbactest"
const chartReleasePrefix = "rbactest-kube-vnet-"

func mustInstallAggregatedRBAC(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping aggregated RBAC test")
	}

	chartDir := filepath.Join("..", "..", "charts", "kube-vnet")
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("helm", "template", chartReleaseName, chartDir,
		"--kube-version", "1.31.0",
		"--show-only", "templates/rbac-aggregated.yaml",
		"--set", "operator.clusterBaseline.ingressIsolationLevel=namespace",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\nstderr: %s", err, stderr.String())
	}

	dec := yaml.NewYAMLOrJSONDecoder(&stdout, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered RBAC: %v", err)
		}
		if obj.Object == nil || obj.GetKind() == "" {
			continue
		}
		toCreate := obj
		if err := testClient.Create(context.Background(), toCreate); err != nil {
			t.Fatalf("install %s/%s: %v", obj.GetKind(), obj.GetName(), err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), toCreate) })
	}
}

// bindUserToRoleInNS creates a RoleBinding granting `user` the
// `<chartReleasePrefix><resource>-editor` ClusterRole within `ns`.
func bindUserToEditorInNS(t *testing.T, user, resource, ns string) {
	t.Helper()
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rbactest-" + sanitizeUser(user) + "-" + resource,
			Namespace: ns,
		},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: user, APIGroup: rbacv1.GroupName}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     chartReleasePrefix + resource + "-editor",
		},
	}
	if err := testClient.Create(context.Background(), rb); err != nil {
		t.Fatalf("create RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), rb) })
}

// bindUserToClusterRole creates a ClusterRoleBinding granting `user` the
// named ClusterRole cluster-wide.
func bindUserToClusterRole(t *testing.T, user, clusterRoleName string) {
	t.Helper()
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rbactest-" + sanitizeUser(user) + "-" + clusterRoleName,
		},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: user, APIGroup: rbacv1.GroupName}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
	}
	if err := testClient.Create(context.Background(), crb); err != nil {
		t.Fatalf("create ClusterRoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), crb) })
}

func TestIntegration_RBAC_NamespaceAdmin_CanCreateBaselineInOwnNS(t *testing.T) {
	mustInstallAggregatedRBAC(t)
	const user = "alice@example.com"
	ns := uniqueNS(t, "rbac-own")
	mustCreate(t, makeNamespace(ns, nil, nil))
	bindUserToEditorInNS(t, user, "virtualnetworkbaselines", ns)

	c := mustImpersonate(t, user)
	nb := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
	}
	if err := c.Create(context.Background(), nb); err != nil {
		t.Fatalf("create in own NS: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), nb) })
}

func TestIntegration_RBAC_NamespaceAdmin_CannotCreateBaselineInOtherNS(t *testing.T) {
	mustInstallAggregatedRBAC(t)
	const user = "alice@example.com"
	bound := uniqueNS(t, "rbac-bound")
	other := uniqueNS(t, "rbac-other")
	mustCreate(t, makeNamespace(bound, nil, nil))
	mustCreate(t, makeNamespace(other, nil, nil))
	bindUserToEditorInNS(t, user, "virtualnetworkbaselines", bound)

	c := mustImpersonate(t, user)
	nb := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: other},
	}
	err := c.Create(context.Background(), nb)
	if err == nil {
		_ = c.Delete(context.Background(), nb)
		t.Fatalf("expected Forbidden in unbound NS, got accept")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden, got: %v", err)
	}
}

func TestIntegration_RBAC_NamespaceAdmin_CannotCreateClusterBaseline(t *testing.T) {
	mustInstallAggregatedRBAC(t)
	const user = "alice@example.com"
	ns := uniqueNS(t, "rbac-noclus")
	mustCreate(t, makeNamespace(ns, nil, nil))
	// Even with the namespace-scoped editor on the cluster-baseline kind name,
	// a RoleBinding cannot grant access to a cluster-scoped resource.
	bindUserToEditorInNS(t, user, "virtualnetworkbaselines", ns)

	c := mustImpersonate(t, user)
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	err := c.Create(context.Background(), cb)
	if err == nil {
		_ = c.Delete(context.Background(), cb)
		t.Fatalf("expected Forbidden on cluster-scoped CR, got accept")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden, got: %v", err)
	}
}

func TestIntegration_RBAC_ClusterAdmin_CanCreateClusterBaseline(t *testing.T) {
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	// testClient is the envtest admin (effectively system:masters).
	if err := testClient.Create(context.Background(), cb); err != nil {
		t.Fatalf("admin create: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), cb) })
}

func TestIntegration_RBAC_DelegatedClusterBaselineEditor_CanCreate(t *testing.T) {
	mustInstallAggregatedRBAC(t)
	const user = "alice@example.com"
	bindUserToClusterRole(t, user, chartReleasePrefix+"clustervirtualnetworkbaselines-editor")

	c := mustImpersonate(t, user)
	cb := &vnetv1alpha1.ClusterVirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	if err := c.Create(context.Background(), cb); err != nil {
		t.Fatalf("delegated create: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cb) })
}

func TestIntegration_RBAC_Viewer_ReadsButCannotWrite(t *testing.T) {
	mustInstallAggregatedRBAC(t)
	const user = "bob@example.com"
	ns := uniqueNS(t, "rbac-view")
	mustCreate(t, makeNamespace(ns, nil, nil))

	// Bind bob to the namespace-baseline VIEWER (not editor) within the NS.
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbactest-bob-view", Namespace: ns},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: user, APIGroup: rbacv1.GroupName}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     chartReleasePrefix + "virtualnetworkbaselines-viewer",
		},
	}
	if err := testClient.Create(context.Background(), rb); err != nil {
		t.Fatalf("create RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), rb) })

	// Seed a baseline as admin so bob has something to read.
	seed := &vnetv1alpha1.VirtualNetworkBaseline{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
	}
	if err := testClient.Create(context.Background(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), seed) })

	c := mustImpersonate(t, user)

	// Read works.
	got := &vnetv1alpha1.VirtualNetworkBaseline{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "default"}, got); err != nil {
		t.Fatalf("viewer Get: %v", err)
	}

	// Write fails Forbidden.
	mut := got.DeepCopy()
	mut.Spec.Memberships = []vnetv1alpha1.BaselineMembership{
		{
			VirtualNetworkRef: vnetv1alpha1.VirtualNetworkRef{Name: "namespace", Namespace: "kube-vnet-system-test"},
			Direction:         "default-both",
		},
	}
	err := c.Update(context.Background(), mut)
	if err == nil {
		t.Fatalf("expected Forbidden on viewer Update, got accept")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden, got: %v", err)
	}
}

func TestIntegration_RBAC_AggregationLabelsPresent(t *testing.T) {
	mustInstallAggregatedRBAC(t)

	cases := []struct {
		role  string
		labels map[string]string
	}{
		{"virtualnetworks-editor", map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true", "rbac.authorization.k8s.io/aggregate-to-edit": "true"}},
		{"virtualnetworks-viewer", map[string]string{"rbac.authorization.k8s.io/aggregate-to-view": "true"}},
		{"virtualnetworkbindings-editor", map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true", "rbac.authorization.k8s.io/aggregate-to-edit": "true"}},
		{"virtualnetworkbindings-viewer", map[string]string{"rbac.authorization.k8s.io/aggregate-to-view": "true"}},
		{"virtualnetworkbaselines-editor", map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true", "rbac.authorization.k8s.io/aggregate-to-edit": "true"}},
		{"virtualnetworkbaselines-viewer", map[string]string{"rbac.authorization.k8s.io/aggregate-to-view": "true"}},
	}
	for _, tc := range cases {
		role := &rbacv1.ClusterRole{}
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: chartReleasePrefix + tc.role}, role); err != nil {
			t.Fatalf("get ClusterRole %s: %v", tc.role, err)
		}
		for k, want := range tc.labels {
			if got := role.Labels[k]; got != want {
				t.Errorf("ClusterRole %s missing label %q=%q (got %q)", tc.role, k, want, got)
			}
		}
	}

	// Cluster-baseline pair should NOT carry aggregation labels.
	for _, suffix := range []string{"clustervirtualnetworkbaselines-editor", "clustervirtualnetworkbaselines-viewer"} {
		role := &rbacv1.ClusterRole{}
		if err := testClient.Get(context.Background(), client.ObjectKey{Name: chartReleasePrefix + suffix}, role); err != nil {
			t.Fatalf("get ClusterRole %s: %v", suffix, err)
		}
		for k := range role.Labels {
			if strings.HasPrefix(k, "rbac.authorization.k8s.io/aggregate-to-") {
				t.Errorf("ClusterRole %s carries aggregation label %q; cluster-baseline must be cluster-admin only by default", suffix, k)
			}
		}
	}
}
