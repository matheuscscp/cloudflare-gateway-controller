// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/controller"
)

func TestGatewayClassReconciler_Accepted(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-accepted",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() {
		_ = testClient.Delete(testCtx, gc)
	})

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonAccepted)))

		supported := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(supported).NotTo(BeNil())
		g.Expect(supported.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(supported.Reason).To(Equal(string(gatewayv1.GatewayClassReasonSupportedVersion)))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))

		// Verify Normal event was emitted.
		e := findEvent(g, "default", gc.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, "", "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(Equal("GatewayClass reconciled"))
		g.Expect(e.Action).To(Equal(apiv1.EventActionReconcile))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_AcceptedWithParametersRef(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-with-params",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "cloudflare-creds",
				Namespace: new(gatewayv1.Namespace(ns.Name)),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonAccepted)))

		supported := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(supported).NotTo(BeNil())
		g.Expect(supported.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_WrongControllerIgnored(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-wrong-controller",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "example.com/other-controller",
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	// Our controller should not set Ready or SupportedVersion conditions.
	// The API server may set a default Accepted=Unknown/Pending condition.
	key := client.ObjectKeyFromObject(gc)
	g.Consistently(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		g.Expect(conditions.Find(result.Status.Conditions, apiv1.ConditionReady)).To(BeNil())
		g.Expect(conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))).To(BeNil())
	}).WithTimeout(2 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_InvalidParametersRefKind(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-bad-kind",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "ConfigMap",
				Name:      "some-config",
				Namespace: new(gatewayv1.Namespace("default")),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("ConfigMap"))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal(string(gatewayv1.GatewayClassReasonInvalidParameters)))

		// Verify Warning event was emitted.
		e := findEvent(g, "default", gc.Name, corev1.EventTypeWarning, string(gatewayv1.GatewayClassReasonInvalidParameters), "", "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("ConfigMap"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_InvalidParametersRefNoNamespace(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-no-ns",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "",
				Kind:  "Secret",
				Name:  "some-secret",
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("namespace"))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_ParametersRefSecretNotFound(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-secret-not-found",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "nonexistent-secret",
				Namespace: new(gatewayv1.Namespace(ns.Name)),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("not found"))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))

		// Verify Warning event was emitted.
		e := findEvent(g, "default", gc.Name, corev1.EventTypeWarning, string(gatewayv1.GatewayClassReasonInvalidParameters), "", "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("not found"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_ParametersRefSecretMissingKeys(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	// Create Secret with only one of the required keys.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "incomplete-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN": []byte("test-token"),
		},
	}
	g.Expect(testClient.Create(testCtx, secret)).To(Succeed())

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-missing-keys",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "incomplete-secret",
				Namespace: new(gatewayv1.Namespace(ns.Name)),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("CLOUDFLARE_ACCOUNT_ID"))

		// SupportedVersion should still be True (version check passes independently).
		supported := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(supported).NotTo(BeNil())
		g.Expect(supported.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_Idempotent(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-idempotent",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	// Wait for Ready=True.
	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Trigger a re-reconciliation by adding an annotation (AnnotationChangedPredicate
	// is configured in the manager). This forces the reconciler to run again and hit
	// the early-return path where no conditions or features changed.
	var latest gatewayv1.GatewayClass
	g.Expect(testClient.Get(testCtx, key, &latest)).To(Succeed())
	latest.Annotations = map[string]string{"test": "trigger-reconcile"}
	g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())

	// The annotation update bumps the ResourceVersion. Record it after the update settles.
	g.Expect(testClient.Get(testCtx, key, &latest)).To(Succeed())
	rv := latest.ResourceVersion

	// Verify no status patch occurred (ResourceVersion unchanged) despite the reconciliation.
	g.Consistently(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		g.Expect(result.ResourceVersion).To(Equal(rv))
	}).WithTimeout(3 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_SecretUpdateTriggersReconcile(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	// Create Secret missing CLOUDFLARE_ACCOUNT_ID.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloudflare-creds-update",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN": []byte("test-token"),
		},
	}
	g.Expect(testClient.Create(testCtx, secret)).To(Succeed())

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-secret-update",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "cloudflare-creds-update",
				Namespace: new(gatewayv1.Namespace(ns.Name)),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	// Wait for Accepted=False.
	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Fix the Secret by adding the missing key.
	var latestSecret corev1.Secret
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(secret), &latestSecret)).To(Succeed())
	latestSecret.Data["CLOUDFLARE_ACCOUNT_ID"] = []byte("test-account-id")
	g.Expect(testClient.Update(testCtx, &latestSecret)).To(Succeed())

	// Wait for the Secret watch to trigger re-reconciliation → Accepted=True, Ready=True.
	// Use a generous timeout because Secret watch propagation can be slow on CI runners.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(60 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_SupportedFeatures(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-features",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		expected := []gatewayv1.SupportedFeature{
			{Name: gatewayv1.FeatureName(features.SupportGateway)},
			{Name: gatewayv1.FeatureName(features.SupportGatewayInfrastructurePropagation)},
			{Name: gatewayv1.FeatureName(features.SupportHTTPRoute)},
			{Name: gatewayv1.FeatureName(features.SupportReferenceGrant)},
		}
		g.Expect(result.Status.SupportedFeatures).To(Equal(expected))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_InvalidParametersRefGroup(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-bad-group",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "apps",
				Kind:      "Secret",
				Name:      "some-secret",
				Namespace: new(gatewayv1.Namespace("default")),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("apps"))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_UnsupportedCRDVersion(t *testing.T) {
	g := NewWithT(t)

	// Temporarily modify the GatewayClass CRD's bundle-version annotation to an
	// incompatible version, then restore it after the test.
	crdKey := client.ObjectKey{Name: apiv1.CRDGatewayClass}
	var crd apiextensionsv1.CustomResourceDefinition
	g.Expect(testClient.Get(testCtx, crdKey, &crd)).To(Succeed())

	originalVersion := crd.Annotations[apiv1.AnnotationBundleVersion]
	g.Expect(originalVersion).NotTo(BeEmpty(), "CRD should have a bundle-version annotation")

	// Set an incompatible version (major version mismatch).
	crd.Annotations[apiv1.AnnotationBundleVersion] = "0.1.0"
	g.Expect(testClient.Update(testCtx, &crd)).To(Succeed())
	t.Cleanup(func() {
		// Restore the original version.
		var latest apiextensionsv1.CustomResourceDefinition
		if err := testClient.Get(testCtx, crdKey, &latest); err == nil {
			latest.Annotations[apiv1.AnnotationBundleVersion] = originalVersion
			_ = testClient.Update(testCtx, &latest)
		}
	})

	// Create a GatewayClass — it should be rejected with UnsupportedVersion.
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-unsupported-crd",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonUnsupportedVersion)))

		sv := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(sv).NotTo(BeNil())
		g.Expect(sv.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(sv.Reason).To(Equal(string(gatewayv1.GatewayClassReasonUnsupportedVersion)))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal(string(gatewayv1.GatewayClassReasonUnsupportedVersion)))
		g.Expect(ready.Message).To(ContainSubstring("not compatible"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_MissingCRDBundleVersionAnnotation(t *testing.T) {
	g := NewWithT(t)

	// Temporarily remove the bundle-version annotation from the GatewayClass CRD.
	crdKey := client.ObjectKey{Name: apiv1.CRDGatewayClass}
	var crd apiextensionsv1.CustomResourceDefinition
	g.Expect(testClient.Get(testCtx, crdKey, &crd)).To(Succeed())

	originalVersion := crd.Annotations[apiv1.AnnotationBundleVersion]
	g.Expect(originalVersion).NotTo(BeEmpty(), "CRD should have a bundle-version annotation")

	delete(crd.Annotations, apiv1.AnnotationBundleVersion)
	g.Expect(testClient.Update(testCtx, &crd)).To(Succeed())
	t.Cleanup(func() {
		var latest apiextensionsv1.CustomResourceDefinition
		if err := testClient.Get(testCtx, crdKey, &latest); err == nil {
			if latest.Annotations == nil {
				latest.Annotations = make(map[string]string)
			}
			latest.Annotations[apiv1.AnnotationBundleVersion] = originalVersion
			_ = testClient.Update(testCtx, &latest)
		}
	})

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-missing-annotation",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonUnsupportedVersion)))
		g.Expect(accepted.Message).To(ContainSubstring("missing"))

		sv := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(sv).NotTo(BeNil())
		g.Expect(sv.Status).To(Equal(metav1.ConditionFalse))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Message).To(ContainSubstring("missing"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_InvalidCRDBundleVersionSemver(t *testing.T) {
	g := NewWithT(t)

	// Temporarily set the bundle-version annotation to an invalid semver string.
	crdKey := client.ObjectKey{Name: apiv1.CRDGatewayClass}
	var crd apiextensionsv1.CustomResourceDefinition
	g.Expect(testClient.Get(testCtx, crdKey, &crd)).To(Succeed())

	originalVersion := crd.Annotations[apiv1.AnnotationBundleVersion]
	g.Expect(originalVersion).NotTo(BeEmpty(), "CRD should have a bundle-version annotation")

	crd.Annotations[apiv1.AnnotationBundleVersion] = "not-a-version"
	g.Expect(testClient.Update(testCtx, &crd)).To(Succeed())
	t.Cleanup(func() {
		var latest apiextensionsv1.CustomResourceDefinition
		if err := testClient.Get(testCtx, crdKey, &latest); err == nil {
			latest.Annotations[apiv1.AnnotationBundleVersion] = originalVersion
			_ = testClient.Update(testCtx, &latest)
		}
	})

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-invalid-semver",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonUnsupportedVersion)))
		g.Expect(accepted.Message).To(ContainSubstring("Failed to parse"))

		sv := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(sv).NotTo(BeNil())
		g.Expect(sv.Status).To(Equal(metav1.ConditionFalse))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Message).To(ContainSubstring("not-a-version"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayClassReconciler_TransientError(t *testing.T) {
	g := NewWithT(t)

	// Create a GatewayClass that the shared reconciler will accept.
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-transient-error",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	// Wait for the shared reconciler to set Ready=True first, so we have a
	// stable baseline before the fault-injected reconciliation.
	waitForGatewayClassReady(g, gc)

	// Build a standalone reconciler with a fault-injecting client that fails
	// Get calls for the GatewayClass CRD metadata, triggering reconcileError.
	fc := &faultClient{
		Client: testClient,
		getInterceptor: func(_ context.Context, key client.ObjectKey, obj client.Object) error {
			if key.Name == apiv1.CRDGatewayClass && key.Namespace == "" {
				if _, ok := obj.(*metav1.PartialObjectMetadata); ok {
					return fmt.Errorf("simulated transient API error")
				}
			}
			return nil
		},
	}
	r := &controller.GatewayClassReconciler{
		Client:            fc,
		EventRecorder:     noopEventRecorder{},
		GatewayAPIVersion: *testGatewayAPIVersion,
	}

	// Call Reconcile directly — this exercises reconcileError.
	result, err := r.Reconcile(testCtx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gc),
	})
	g.Expect(err).To(MatchError(ContainSubstring("simulated transient API error")))
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify status was best-effort patched with Ready=Unknown/ProgressingWithRetry.
	var latest gatewayv1.GatewayClass
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest)).To(Succeed())
	ready := conditions.Find(latest.Status.Conditions, apiv1.ConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
	g.Expect(ready.Message).To(ContainSubstring("simulated transient API error"))
}

func TestGatewayClassReconciler_TransientErrorStatusPatchFails(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-transient-patch-fail",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, gc) })

	waitForGatewayClassReady(g, gc)

	// Fault-injecting client that fails both the CRD lookup (triggering
	// reconcileError) AND the status patch (testing the error logging path
	// inside reconcileError when the best-effort patch itself fails).
	fc := &faultClient{
		Client: testClient,
		getInterceptor: func(_ context.Context, key client.ObjectKey, obj client.Object) error {
			if key.Name == apiv1.CRDGatewayClass && key.Namespace == "" {
				if _, ok := obj.(*metav1.PartialObjectMetadata); ok {
					return fmt.Errorf("simulated CRD fetch error")
				}
			}
			return nil
		},
		statusPatchInterceptor: func(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			return fmt.Errorf("simulated status patch error")
		},
	}
	r := &controller.GatewayClassReconciler{
		Client:            fc,
		EventRecorder:     noopEventRecorder{},
		GatewayAPIVersion: *testGatewayAPIVersion,
	}

	// Call Reconcile directly — reconcileError should still return the original
	// error even though the best-effort status patch failed.
	result, err := r.Reconcile(testCtx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gc),
	})
	g.Expect(err).To(MatchError(ContainSubstring("simulated CRD fetch error")))
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Status should still be Ready=True (the best-effort patch failed, so
	// the previous state from the shared reconciler is preserved).
	var latest gatewayv1.GatewayClass
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest)).To(Succeed())
	ready := conditions.Find(latest.Status.Conditions, apiv1.ConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}
