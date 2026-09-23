//go:build integration

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// TestIntegration_VAP_SystemVnetProtected: the system-vnet-protected VAP
// rejects user CREATEs of the reserved names and the system label, and admits
// the operator ServiceAccount for the same shapes. The rest of the suite runs
// without any VAP; this one is installed only for this test.
//
// operatorUserName is what the chart defaults render the VAP's operatorUser
// expression to; update it if the chart's namespace/SA defaults change.
const operatorUserName = "system:serviceaccount:kube-vnet-system:kube-vnet-controller"

func TestIntegration_VAP_SystemVnetProtected(t *testing.T) {
	ctx := context.Background()

	mustInstallKustomizeVAP(t, "system-vnet-vap.yaml")
	mustGrantRBAC(t, "vap-test-vnet-rw", "kube-vnet.lhns.de", "virtualnetworks", "alice@example.com", operatorUserName)

	userClient := mustImpersonate(t, "alice@example.com")
	opClient := mustImpersonate(t, operatorUserName)

	// Use a disabled namespace so SystemVnetReconciler doesn't auto-create
	// the per-namespace `namespace` vnet — otherwise the operator-SA subtest's
	// own create races with that one.
	ns := uniqueNS(t, "vap")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	awaitVAPActive(t, userClient, func() client.Object {
		return &vnetv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{Name: "namespace", Namespace: ns}}
	})

	t.Run("user creating VirtualNetwork named `namespace` is rejected", func(t *testing.T) {
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "namespace"
		v.Namespace = ns
		err := userClient.Create(ctx, v)
		if err == nil {
			_ = userClient.Delete(ctx, v)
			t.Fatalf("expected VAP rejection, got accept")
		}
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("expected Invalid w/ reserved-names message, got: %v", err)
		}
	})

	t.Run("user creating VirtualNetwork named `cluster` is rejected", func(t *testing.T) {
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "cluster"
		v.Namespace = ns
		err := userClient.Create(ctx, v)
		if err == nil {
			_ = userClient.Delete(ctx, v)
			t.Fatalf("expected VAP rejection, got accept")
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("expected Invalid, got: %v", err)
		}
	})

	t.Run("user creating VirtualNetwork with kube-vnet.system/managed-by=kube-vnet is rejected", func(t *testing.T) {
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "spoof"
		v.Namespace = ns
		v.Labels = map[string]string{LabelManagedBy: LabelManagedByValue}
		err := userClient.Create(ctx, v)
		if err == nil {
			_ = userClient.Delete(ctx, v)
			t.Fatalf("expected VAP rejection, got accept")
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("expected Invalid, got: %v", err)
		}
	})

	t.Run("user creating a normal VirtualNetwork is accepted", func(t *testing.T) {
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "ordinary"
		v.Namespace = ns
		if err := userClient.Create(ctx, v); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), v) })
	})

	t.Run("operator SA can create the otherwise-reserved shapes", func(t *testing.T) {
		// name=cluster + system label — this is exactly what
		// SystemVnetReconciler does in production.
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "cluster"
		v.Namespace = ns
		v.Labels = map[string]string{LabelManagedBy: LabelManagedByValue}
		if err := opClient.Create(ctx, v); err != nil {
			t.Fatalf("operator SA create: %v", err)
		}
		t.Cleanup(func() { _ = opClient.Delete(context.Background(), v) })

		v2 := &vnetv1alpha1.VirtualNetwork{}
		v2.Name = "namespace"
		v2.Namespace = ns
		v2.Labels = map[string]string{LabelManagedBy: LabelManagedByValue}
		if err := opClient.Create(ctx, v2); err != nil {
			t.Fatalf("operator SA create namespace-vnet: %v", err)
		}
		t.Cleanup(func() { _ = opClient.Delete(context.Background(), v2) })
	})

	// DELETE must stay unguarded so the namespace controller can cascade-delete
	// the `namespace` system vnet (guarding it left namespaces stuck in
	// Terminating); drift correction recreates a vnet a user deletes. The vnet
	// carries the protected label but an ordinary name, so the disabled-namespace
	// cleanup of the vnet named `namespace` can't race the delete.
	t.Run("user DELETE of a system-labeled vnet is not blocked", func(t *testing.T) {
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "labeled-ordinary"
		v.Namespace = ns
		v.Labels = map[string]string{LabelManagedBy: LabelManagedByValue}
		if err := opClient.Create(ctx, v); err != nil {
			t.Fatalf("operator SA create: %v", err)
		}
		// A VAP denial would be Invalid/Forbidden; NotFound would mean
		// something already deleted it (also "not blocked"). Only an
		// admission denial is a failure.
		if err := userClient.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("expected DELETE to be allowed (VAP must not guard DELETE), got: %v", err)
		}
	})
}

// mustInstallKustomizeVAP installs config/admission/<file> (a VAP and its
// Binding) for the duration of the test. The kustomize copy is rendered from
// the chart, so this exercises the same bytes without needing helm.
func mustInstallKustomizeVAP(t *testing.T, file string) {
	t.Helper()
	path := filepath.Join("..", "..", "config", "admission", file)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	objs := decodeObjects(t, f)
	if len(objs) < 2 {
		t.Fatalf("expected VAP + Binding in %s, got %d objects", path, len(objs))
	}
	for _, obj := range objs {
		if err := testClient.Create(context.Background(), obj); err != nil {
			t.Fatalf("install %s/%s: %v", obj.GetKind(), obj.GetName(), err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })
	}
}

// mustGrantRBAC creates a ClusterRole `name` granting CRUD on
// apiGroup/resource, bound to each user, removed on cleanup. Without it the
// apiserver answers Forbidden before any VAP runs.
func mustGrantRBAC(t *testing.T, name, apiGroup, resource string, users ...string) {
	t.Helper()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{apiGroup},
			Resources: []string{resource},
			Verbs:     []string{"create", "get", "list", "update", "patch", "delete"},
		}},
	}
	if err := testClient.Create(context.Background(), role); err != nil {
		t.Fatalf("create ClusterRole: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), role) })

	for _, user := range users {
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-" + sanitizeUser(user)},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: user, APIGroup: rbacv1.GroupName}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     name,
			},
		}
		if err := testClient.Create(context.Background(), binding); err != nil {
			t.Fatalf("create ClusterRoleBinding for %s: %v", user, err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), binding) })
	}
}

func sanitizeUser(u string) string {
	r := strings.NewReplacer("@", "-", ":", "-", ".", "-")
	return strings.ToLower(r.Replace(u))
}

// mustImpersonate returns a client whose REST calls impersonate `user`.
// Tests use envtest's admin credentials underneath, but admission-time
// `request.userInfo.username` is what the VAP CEL evaluates.
func mustImpersonate(t *testing.T, user string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(testCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("impersonated client (%s): %v", user, err)
	}
	return c
}

// awaitVAPActive retries creating a probe object the VAP must reject until it
// does: a freshly installed VAP is not enforced on the very next request.
// A probe that gets admitted is deleted so it can't collide with a subtest.
func awaitVAPActive(t *testing.T, c client.Client, probe func() client.Object) {
	t.Helper()
	eventually(t, 10*time.Second, func() error {
		obj := probe()
		err := c.Create(context.Background(), obj)
		if err == nil {
			_ = c.Delete(context.Background(), obj)
			return fmt.Errorf("VAP not active yet: probe %s was admitted", obj.GetName())
		}
		if !apierrors.IsInvalid(err) {
			return fmt.Errorf("VAP not active yet: %v", err)
		}
		return nil
	})
}

// The join-label direction VAP. It previously denied any pod with no labels,
// because an unguarded object.metadata.labels raises `no such key: labels` and
// failurePolicy: Fail turns that into a denial. See ADR 0027.
func TestIntegration_VAP_JoinLabelDirection(t *testing.T) {
	ctx := context.Background()

	mustInstallKustomizeVAP(t, "validating-admission-policy.yaml")
	mustGrantRBAC(t, "vap-test-pod-rw", "", "pods", "alice@example.com")
	userClient := mustImpersonate(t, "alice@example.com")

	ns := uniqueNS(t, "vapdir")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	awaitVAPActive(t, userClient, func() client.Object {
		return makePod(ns, "vap-probe", map[string]string{"kube-vnet/net.probe": "true"})
	})

	// The bug. A pod with no labels field at all must be admitted.
	t.Run("pod with no labels is accepted", func(t *testing.T) {
		p := makePod(ns, "nolabels", nil)
		if err := userClient.Create(ctx, p); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), p) })
	})

	// Unstructured because the typed client drops an empty map (omitempty).
	// The apiserver normalises it back to absent, so this is a guard in case
	// that ever changes.
	t.Run("pod with an empty labels map is accepted", func(t *testing.T) {
		p := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name": "emptylabels", "namespace": ns,
				"labels": map[string]interface{}{},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{map[string]interface{}{
					"name": "app", "image": "registry.k8s.io/pause:3.10",
				}},
			},
		}}
		if err := userClient.Create(ctx, p); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), p) })
	})

	t.Run("pod with unrelated labels is accepted", func(t *testing.T) {
		p := makePod(ns, "unrelated", map[string]string{"foo": "bar"})
		if err := userClient.Create(ctx, p); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), p) })
	})

	t.Run("pod with a valid join label is accepted", func(t *testing.T) {
		p := makePod(ns, "validdir", map[string]string{"kube-vnet/net.x": "both"})
		if err := userClient.Create(ctx, p); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), p) })
	})

	// Must pass before and after the fix: guarding the lookup must not disable
	// the check itself.
	t.Run("pod with a removed legacy direction value is rejected", func(t *testing.T) {
		p := makePod(ns, "legacydir", map[string]string{"kube-vnet/net.x": "true"})
		err := userClient.Create(ctx, p)
		if err == nil {
			_ = userClient.Delete(ctx, p)
			t.Fatal("expected the VAP to reject the legacy `true` direction value")
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("expected an admission rejection, got: %v", err)
		}
		if !strings.Contains(err.Error(), "both, ingress, egress, none") {
			t.Errorf("rejection should name the valid values, got: %v", err)
		}
	})

	// The UPDATE half of matchConstraints, and the operator's own path:
	// applyResolution patches every pod to set resolved-generation.
	t.Run("updating a label-less pod is accepted", func(t *testing.T) {
		p := makePod(ns, "updatable", nil)
		if err := userClient.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), p) })

		patched := p.DeepCopy()
		patched.Annotations = map[string]string{AnnotationResolvedGeneration: "1"}
		if err := userClient.Patch(ctx, patched, client.MergeFrom(p)); err != nil {
			t.Fatalf("expected the annotation patch to be accepted, got: %v", err)
		}
	})
}
