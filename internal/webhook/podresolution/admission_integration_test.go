//go:build integration

package podresolution

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vnetv1alpha1 "github.com/lhns/kube-vnet/api/v1alpha1"
	"github.com/lhns/kube-vnet/internal/controller"
)

// These tests exercise the admission path through a real apiserver: the
// handlers are registered on the manager's webhook server and the
// configurations below are installed by envtest, which rewrites clientConfig
// to the local serving endpoint and injects its own CA.
//
// The oracle is the create response. testClient.Create writes the apiserver's
// returned object back into the pod, so a system label visible there was
// applied during admission — not moments later by the reconciler. Asserting
// under eventually() instead would pass on the controller path and prove
// nothing, which is the whole point of the negative control below.
//
// The configurations are scoped by namespace label rather than using the
// chart's selectors, so only tests that opt in are affected. The validating
// side runs failurePolicy: Fail; left cluster-wide, a webhook server that is
// slow to come up would fail every pod creation in the whole suite. The
// chart's real selectors are covered by the chart-render tests.

const webhookOptInLabel = "kube-vnet-webhook-test"

func webhookNamespaceSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{webhookOptInLabel: "true"}}
}

func webhookPodRules() []admissionregistrationv1.RuleWithOperations {
	scope := admissionregistrationv1.NamespacedScope
	return []admissionregistrationv1.RuleWithOperations{{
		Operations: []admissionregistrationv1.OperationType{
			admissionregistrationv1.Create, admissionregistrationv1.Update,
		},
		Rule: admissionregistrationv1.Rule{
			APIGroups:   []string{""},
			APIVersions: []string{"v1"},
			Resources:   []string{"pods"},
			Scope:       &scope,
		},
	}}
}

func testMutatingWebhook() *admissionregistrationv1.MutatingWebhookConfiguration {
	path := MutatePath
	sideEffects := admissionregistrationv1.SideEffectClassNone
	failurePolicy := admissionregistrationv1.Ignore
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vnet-test-pod-resolution"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name:                    "pods.resolution.kube-vnet.lhns.de",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &failurePolicy,
			NamespaceSelector:       webhookNamespaceSelector(),
			Rules:                   webhookPodRules(),
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Name: "kube-vnet-webhook", Namespace: "default", Path: &path,
				},
			},
		}},
	}
}

func testValidatingWebhook() *admissionregistrationv1.ValidatingWebhookConfiguration {
	path := ValidatePath
	sideEffects := admissionregistrationv1.SideEffectClassNone
	failurePolicy := admissionregistrationv1.Fail
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vnet-test-pod-resolution"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name:                    "pods.systemlabels.kube-vnet.lhns.de",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &failurePolicy,
			NamespaceSelector:       webhookNamespaceSelector(),
			Rules:                   webhookPodRules(),
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Name: "kube-vnet-webhook", Namespace: "default", Path: &path,
				},
			},
		}},
	}
}

// webhookNS creates a namespace opted into the webhook plus a vnet in it, and
// waits until admission is actually being served. A freshly installed
// configuration is not enforced on the very next request, and the webhook
// server needs a moment to bind — without this gate the first test to run
// races both.
func webhookNS(t *testing.T, vnetName string) string {
	t.Helper()
	ns := uniqueNS(t, "wh")
	mustCreate(t, makeNamespace(ns, nil, map[string]string{webhookOptInLabel: "true"}))
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: vnetName},
	})

	eventually(t, 60*time.Second, func() error {
		p := makePod(ns, "probe-"+rand.String(5),
			map[string]string{"kube-vnet/net." + vnetName: "both"})
		if err := testClient.Create(context.Background(), p); err != nil {
			return fmt.Errorf("probe create: %w", err)
		}
		defer func() { _ = testClient.Delete(context.Background(), p) }()
		if _, ok := p.Labels[controller.LabelSystemNetPrefix+ns+"."+vnetName]; !ok {
			return fmt.Errorf("webhook not stamping yet (labels %v)", p.Labels)
		}
		return nil
	})
	return ns
}

// The headline property: the pod is a member before it exists, not shortly
// after. If this regresses, the race that motivated ADR 0034 is back.
func TestIntegration_Webhook_StampsDuringAdmission(t *testing.T) {
	ns := webhookNS(t, "web")
	want := controller.LabelSystemNetPrefix + ns + ".web"

	pod := makePod(ns, "stamped", map[string]string{"kube-vnet/net.web": "both"})
	mustCreate(t, pod)

	if got := pod.Labels[want]; got != "both" {
		t.Fatalf("the create response carried %q=%q, want %q.\n"+
			"A stamp that appears only later is the resolution race this webhook exists "+
			"to eliminate.\nfull labels: %v", want, got, "both", pod.Labels)
	}
	if by := pod.Annotations[controller.AnnotationResolvedBy]; by != controller.ResolvedByAdmission {
		t.Errorf("resolved-by = %q, want %q", by, controller.ResolvedByAdmission)
	}
	if pod.Annotations[controller.AnnotationResolvedGeneration] == "" {
		t.Error("resolved-generation annotation missing; membership generation skips pods without it")
	}
}

// The negative control that gives the test above its meaning: in a namespace
// the webhook does not cover, the same pod is not stamped at creation and only
// becomes a member once the reconciler catches up.
func TestIntegration_Webhook_WithoutWebhook_StampArrivesLate(t *testing.T) {
	ns := uniqueNS(t, "nowh")
	mustCreate(t, makeNamespace(ns, nil, nil)) // no opt-in label
	mustCreate(t, &vnetv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
	})
	want := controller.LabelSystemNetPrefix + ns + ".web"

	pod := makePod(ns, "late", map[string]string{"kube-vnet/net.web": "both"})
	mustCreate(t, pod)

	if _, ok := pod.Labels[want]; ok {
		t.Fatalf("pod was stamped at admission in a namespace the webhook does not cover; "+
			"the opt-in test above would then prove nothing (labels %v)", pod.Labels)
	}

	eventually(t, 30*time.Second, func() error {
		var got corev1.Pod
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &got); err != nil {
			return err
		}
		if got.Labels[want] != "both" {
			return fmt.Errorf("controller has not stamped yet: %v", got.Labels)
		}
		return nil
	})
}

// The interaction that makes the design shippable. The mutator writes
// kube-vnet.system/* labels attributed to the requesting user, and the
// validator judges the result. If they disagree, every pod creation in a
// managed namespace fails — which is exactly what the system-labels VAP would
// have done in the validator's place.
func TestIntegration_Webhook_MutatorOutputSurvivesValidator(t *testing.T) {
	ns := webhookNS(t, "web")

	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"member", map[string]string{"kube-vnet/net.web": "both"}},
		{"non-member", map[string]string{"app": "x"}},
		{"ingress only", map[string]string{"kube-vnet/net.web": "ingress"}},
		{"opted out", map[string]string{"kube-vnet/net.web": "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := makePod(ns, "mv-"+tc.name[:3], tc.labels)
			if err := testClient.Create(context.Background(), pod); err != nil {
				t.Fatalf("the validator rejected the mutator's own output: %v\n"+
					"this shape of failure blocks every pod creation in a managed namespace", err)
			}
			_ = testClient.Delete(context.Background(), pod)
		})
	}
}

// Forging a membership stamp must be impossible, which is what the validating
// webhook buys back after the VAP's pods rule is dropped.
func TestIntegration_Webhook_ForgedStampRejected(t *testing.T) {
	ns := webhookNS(t, "web")

	// A stamp for a vnet the pod does not resolve into.
	pod := makePod(ns, "forged", map[string]string{
		controller.LabelSystemNetPrefix + ns + ".secret": "both",
	})
	err := testClient.Create(context.Background(), pod)
	if err == nil {
		t.Fatal("a forged kube-vnet.system membership stamp was admitted")
	}
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		t.Fatalf("expected a denial, got %T: %v", err, err)
	}
}

// A wrong value for a vnet the pod really is in is also forgery. Without the
// webhook the admission policy rejects it; with it, the mutator leaves the
// request's value alone so the validator rejects it the same way, rather than
// the value being silently corrected.
func TestIntegration_Webhook_WrongValueForMemberVnetRejected(t *testing.T) {
	ns := webhookNS(t, "web")

	pod := makePod(ns, "wrong-value", map[string]string{
		"kube-vnet/net.web":                           "both",
		controller.LabelSystemNetPrefix + ns + ".web": "egress",
	})
	err := testClient.Create(context.Background(), pod)
	if err == nil {
		t.Fatalf("a wrong stamp value was admitted (labels %v)", pod.Labels)
	}
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `resolves to "both"`) {
		t.Fatalf("expected the validator's denial naming the resolved value, got %v", err)
	}
}

// Relabelling a running pod is the half no scheduling-gate design could
// cover, and stamp removal is the security-relevant direction: a membership
// that outlives its join label is a grant nobody asked for.
func TestIntegration_Webhook_UpdateRestampsAndPrunes(t *testing.T) {
	ns := webhookNS(t, "web")
	key := controller.LabelSystemNetPrefix + ns + ".web"

	pod := makePod(ns, "relabel", map[string]string{"app": "x"})
	mustCreate(t, pod)
	if _, ok := pod.Labels[key]; ok {
		t.Fatalf("pod was stamped without a join label: %v", pod.Labels)
	}

	pod.Labels["kube-vnet/net.web"] = "both"
	if err := testClient.Update(context.Background(), pod); err != nil {
		t.Fatalf("update adding join label: %v", err)
	}
	if got := pod.Labels[key]; got != "both" {
		t.Fatalf("update response carried %q=%q, want \"both\": stamping must be "+
			"synchronous on UPDATE too (labels %v)", key, got, pod.Labels)
	}

	delete(pod.Labels, "kube-vnet/net.web")
	if err := testClient.Update(context.Background(), pod); err != nil {
		t.Fatalf("update removing join label: %v", err)
	}
	if _, ok := pod.Labels[key]; ok {
		t.Fatalf("the stamp outlived its join label: %v", pod.Labels)
	}
}

// A pod stamped at admission must reconcile to an identical label set, so the
// controller has nothing to write. Without this, enabling the webhook would
// add an API write per pod per reconcile — the churn the 0.7.x work removed,
// and nothing else in the suite would notice.
func TestIntegration_Webhook_ReconcileAfterAdmissionIsNoOp(t *testing.T) {
	ns := webhookNS(t, "web")

	pod := makePod(ns, "noop", map[string]string{"kube-vnet/net.web": "both"})
	mustCreate(t, pod)
	if pod.Annotations[controller.AnnotationResolvedBy] != controller.ResolvedByAdmission {
		t.Fatalf("expected an admission stamp, got resolved-by=%q", pod.Annotations[controller.AnnotationResolvedBy])
	}
	rv := pod.ResourceVersion

	// Give the resolution controller time to observe the pod and decide.
	time.Sleep(2 * time.Second)

	var got corev1.Pod
	if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got.ResourceVersion != rv {
		t.Errorf("the pod was written again after admission stamped it "+
			"(resourceVersion %s -> %s, resolved-by=%q).\nA webhook-stamped pod must "+
			"reconcile to a no-op or every pod costs an extra write per reconcile.",
			rv, got.ResourceVersion, got.Annotations[controller.AnnotationResolvedBy])
	}
}
