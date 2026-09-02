//go:build integration

package controller

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
)

// TestIntegration_VAP_SystemVnetProtected verifies the chart's
// system-vnet-protected ValidatingAdmissionPolicy actually rejects user
// CREATE attempts on the reserved names and the system label, and admits
// the operator ServiceAccount for the same shapes.
//
// Installs the rendered VAP from config/admission/system-vnet-vap.yaml so
// the test fails on either chart-side regressions or kustomize-render
// drift. The other suite tests run with no VAP installed (TestMain doesn't
// install one), so the VAP only affects this test's window; t.Cleanup
// removes it on exit.
//
// The VAP template's operatorUser expression is rendered to the literal
// `system:serviceaccount:kube-vnet-system:kube-vnet-controller` by the
// chart defaults — kept in sync with the value below; if you change the
// chart's release-namespace/SA defaults, update operatorUserName too.
const operatorUserName = "system:serviceaccount:kube-vnet-system:kube-vnet-controller"

func TestIntegration_VAP_SystemVnetProtected(t *testing.T) {
	ctx := context.Background()

	policyObjs := mustLoadVAPFromKustomize(t, "system-vnet-vap.yaml")
	for _, obj := range policyObjs {
		obj := obj
		if err := testClient.Create(ctx, obj); err != nil {
			t.Fatalf("install %s/%s: %v", obj.GetKind(), obj.GetName(), err)
		}
		t.Cleanup(func() {
			_ = testClient.Delete(context.Background(), obj)
		})
	}

	// Grant the impersonated identities RBAC for VirtualNetwork CRUD —
	// otherwise the apiserver returns Forbidden before the VAP runs.
	mustGrantVnetRBAC(t, "alice@example.com", operatorUserName)

	userClient := mustImpersonate(t, "alice@example.com")
	opClient := mustImpersonate(t, operatorUserName)

	// Use a disabled namespace so SystemVnetReconciler doesn't auto-create
	// the per-namespace `namespace` vnet — otherwise the operator-SA subtest's
	// own create races with that one.
	ns := uniqueNS(t, "vap")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	// VAPs are not active immediately after install; envtest's apiserver
	// needs a beat to load them. Probe by trying a known-rejected create
	// until the policy fires.
	awaitPolicyActive(t, userClient, ns)

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

	// DELETE is intentionally NOT in this VAP's matchConstraints — it must
	// stay open so the Kubernetes namespace controller can cascade-delete the
	// `namespace` system vnet during namespace teardown (guarding DELETE left
	// every managed namespace stuck in Terminating). A non-operator user
	// deleting a system vnet is recovered by SystemVnetReconciler
	// drift-correction, not by admission.
	//
	// The check uses a system-LABELED vnet with an ORDINARY name: it carries
	// the protected `kube-vnet.system/managed-by` label (so if DELETE were
	// still guarded the VAP would deny it), but its name isn't a reserved
	// system-vnet name, so the SystemVnetReconciler's disabled-namespace
	// cleanup (which only deletes the vnet named exactly `namespace`) never
	// races us. A reserved-name delete would exercise the same now-unmatched
	// operation but race that cleanup, adding no coverage.
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

// mustLoadVAPFromKustomize decodes config/admission/system-vnet-vap.yaml as
// a slice of unstructured objects (the VAP and its Binding). Reading the
// kustomize-rendered file rather than re-rendering via `helm template`
// keeps the test free of a helm-on-PATH dependency and exercises the same
// bytes that a kustomize-installed user would apply.
func mustLoadVAPFromKustomize(t *testing.T, file string) []*unstructured.Unstructured {
	t.Helper()
	path := filepath.Join("..", "..", "config", "admission", file)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		if obj.Object == nil || obj.GetKind() == "" {
			continue
		}
		out = append(out, obj)
	}
	if len(out) < 2 {
		t.Fatalf("expected VAP + Binding in %s, got %d objects", path, len(out))
	}
	return out
}

// mustGrantVnetRBAC creates a ClusterRole + ClusterRoleBinding allowing
// each `user` to CRUD VirtualNetworks. Cleaned up via t.Cleanup. Without
// this the apiserver short-circuits with Forbidden before the VAP runs,
// and we'd never observe the admission-time rejection we're trying to
// test.
func mustGrantVnetRBAC(t *testing.T, users ...string) {
	t.Helper()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "vap-test-vnet-rw"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"kube-vnet.lhns.de"},
			Resources: []string{"virtualnetworks"},
			Verbs:     []string{"create", "get", "list", "update", "patch", "delete"},
		}},
	}
	if err := testClient.Create(context.Background(), role); err != nil {
		t.Fatalf("create ClusterRole: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), role) })

	for _, user := range users {
		user := user
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vap-test-vnet-rw-" + sanitizeUser(user)},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: user, APIGroup: rbacv1.GroupName}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "vap-test-vnet-rw",
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

// awaitPolicyActive retries a known-rejected create until the VAP fires.
// Envtest's apiserver does not enforce a freshly-installed VAP on the very
// next request — there's a tiny propagation window.
func awaitPolicyActive(t *testing.T, c client.Client, ns string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := &vnetv1alpha1.VirtualNetwork{}
		v.Name = "namespace"
		v.Namespace = ns
		err := c.Create(context.Background(), v)
		if err != nil && apierrors.IsInvalid(err) {
			return
		}
		// If create unexpectedly succeeded, clean up so a later subtest
		// doesn't conflict.
		if err == nil {
			_ = c.Delete(context.Background(), v)
		}
		if time.Now().After(deadline) {
			t.Fatalf("VAP did not become active within deadline (last err: %v)", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestIntegration_VAP_JoinLabelDirection covers the join-label direction VAP,
// which had no test at all — which is how it shipped rejecting every pod that
// carries no labels.
//
// In CEL, `object.metadata.labels` on an object with no labels field does not
// evaluate to an empty map; it raises `no such key: labels`. With
// failurePolicy: Fail that evaluation error becomes a denial, so a pod which
// trivially satisfies the rule (no join labels to check) was refused before the
// expression could reach that conclusion. matchConstraints carries no namespace
// or object selector, so this hit every label-less pod in every namespace on
// both CREATE and UPDATE.
//
// Real workloads were spared only because Deployments/StatefulSets/DaemonSets
// must set labels for their own selectors; bare debug pods and label-less Jobs
// were not.
func TestIntegration_VAP_JoinLabelDirection(t *testing.T) {
	ctx := context.Background()

	policyObjs := mustLoadVAPFromKustomize(t, "validating-admission-policy.yaml")
	for _, obj := range policyObjs {
		obj := obj
		if err := testClient.Create(ctx, obj); err != nil {
			t.Fatalf("install %s/%s: %v", obj.GetKind(), obj.GetName(), err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })
	}

	mustGrantPodRBAC(t, "alice@example.com")
	userClient := mustImpersonate(t, "alice@example.com")

	ns := uniqueNS(t, "vapdir")
	mustCreate(t, makeNamespace(ns, map[string]string{"kube-vnet/disabled": "true"}, nil))

	awaitDirectionPolicyActive(t, userClient, ns)

	// The bug. A pod with no labels field at all must be admitted.
	t.Run("pod with no labels is accepted", func(t *testing.T) {
		p := makePod(ns, "nolabels", nil)
		if err := userClient.Create(ctx, p); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
		t.Cleanup(func() { _ = userClient.Delete(context.Background(), p) })
	})

	// Sent as unstructured so `labels: {}` genuinely goes over the wire (the
	// typed client drops an empty map via omitempty). The apiserver then
	// normalises it back to absent, so this reaches CEL as the same case as
	// above and failed identically -- kept as a guard in case that
	// normalisation ever changes.
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

	// REGRESSION LOCK. Guarding the labels lookup must not disable the check
	// itself — the obvious wrong way to fix the bug above. This case must pass
	// both before and after the fix.
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

	// The UPDATE half of matchConstraints, which nothing covered. This is also
	// the operator's own path: applyResolution patches every pod in a managed
	// namespace at least once to set resolved-generation, and this policy —
	// unlike both sibling VAPs — has no operator exemption. While labels were
	// dereferenced unguarded, that patch was denied too, wedging the
	// ResolutionReconciler in a permanent error loop for every label-less pod.
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

// awaitDirectionPolicyActive probes with a known-rejected pod until the policy
// is loaded; VAPs are not active the instant they are created.
func awaitDirectionPolicyActive(t *testing.T, c client.Client, ns string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p := makePod(ns, "vap-probe", map[string]string{"kube-vnet/net.probe": "true"})
		err := c.Create(context.Background(), p)
		if err != nil && apierrors.IsInvalid(err) {
			return
		}
		if err == nil {
			_ = c.Delete(context.Background(), p)
		}
		if time.Now().After(deadline) {
			t.Fatalf("direction VAP did not become active within deadline (last err: %v)", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// mustGrantPodRBAC mirrors mustGrantVnetRBAC for pods: without it the apiserver
// returns Forbidden before the VAP ever runs.
func mustGrantPodRBAC(t *testing.T, users ...string) {
	t.Helper()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "vap-test-pod-rw"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"create", "get", "list", "update", "patch", "delete"},
		}},
	}
	if err := testClient.Create(context.Background(), role); err != nil {
		t.Fatalf("create ClusterRole: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), role) })

	for _, user := range users {
		user := user
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vap-test-pod-rw-" + sanitizeUser(user)},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: user, APIGroup: rbacv1.GroupName}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "vap-test-pod-rw",
			},
		}
		if err := testClient.Create(context.Background(), binding); err != nil {
			t.Fatalf("create ClusterRoleBinding for %s: %v", user, err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), binding) })
	}
}
