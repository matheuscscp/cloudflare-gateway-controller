// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/controller"
)

func TestGatewayReconciler_AcceptedAndProgrammed(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-happy", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed, then verify all conditions
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())

		// Gateway-level conditions
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))

		// Listener status
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		ls := result.Status.Listeners[0]
		g.Expect(ls.Name).To(Equal(gatewayv1.SectionName("http")))
		listenerAccepted := conditions.Find(ls.Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(listenerAccepted).NotTo(BeNil())
		g.Expect(listenerAccepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify tunnel token Secret created
	var tokenSecret corev1.Secret
	secretKey := client.ObjectKey{Name: "cloudflared-token-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())
	g.Expect(tokenSecret.Data).To(HaveKey("TUNNEL_TOKEN"))

	// Verify Deployment created
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(controller.DefaultCloudflaredImage))

	// Verify env uses secretKeyRef, not plain Value
	env := deploy.Spec.Template.Spec.Containers[0].Env
	g.Expect(env).To(HaveLen(1))
	g.Expect(env[0].Name).To(Equal("TUNNEL_TOKEN"))
	g.Expect(env[0].Value).To(BeEmpty())
	g.Expect(env[0].ValueFrom).NotTo(BeNil())
	g.Expect(env[0].ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal("cloudflared-token-" + gw.Name))
	g.Expect(env[0].ValueFrom.SecretKeyRef.Key).To(Equal("TUNNEL_TOKEN"))

	// Verify GatewayClass has the finalizer
	var gcResult gatewayv1.GatewayClass
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &gcResult)).To(Succeed())
	g.Expect(gcResult.Finalizers).To(ContainElement(apiv1.FinalizerGatewayClass(gw)))

	// Verify Normal/ReconciliationSucceeded/Reconcile event was emitted for the Gateway.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, apiv1.EventActionReconcile, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).NotTo(BeEmpty())
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_UnsupportedProtocol(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-unsupported", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-tcp",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "tcp",
					Protocol: gatewayv1.TCPProtocolType,
					Port:     8080,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())

		// Gateway should still be Programmed
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))

		// Listener Accepted=False with UnsupportedProtocol
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		listenerAccepted := conditions.Find(result.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(listenerAccepted).NotTo(BeNil())
		g.Expect(listenerAccepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(listenerAccepted.Reason).To(Equal(string(gatewayv1.ListenerReasonUnsupportedProtocol)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_Deletion(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-deletion", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-delete",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())

	// Wait for Gateway to be Programmed
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Delete the Gateway
	var latest gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
	g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())

	// Wait for Gateway to be fully deleted
	g.Eventually(func() error {
		return testClient.Get(testCtx, gwKey, &latest)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Satisfy(apierrors.IsNotFound))

	// Verify mock's DeleteTunnel was called
	g.Expect(testMock.deleteCalled).To(BeTrue())

	// Verify GatewayClass finalizer was removed (no more gateways)
	g.Eventually(func() []string {
		var gcResult gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &gcResult); err != nil {
			return []string{err.Error()}
		}
		return gcResult.Finalizers
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).ShouldNot(ContainElement(apiv1.FinalizerGatewayClass(gw)))

	// Verify Normal/ReconciliationSucceeded/Finalize event ("Gateway finalized").
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(Equal("Gateway finalized"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CrossNamespaceSecret(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the Secret lives
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// Namespace B: where the Gateway lives
	nsB := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-ns", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-cross-ns",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the Gateway gets Accepted=False due to missing ReferenceGrant
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("not allowed by any ReferenceGrant"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create a ReferenceGrant in namespace A allowing Gateways from namespace B
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-gateway-secret",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1beta1.Group("gateway.networking.k8s.io"),
					Kind:      gatewayv1beta1.Kind("Gateway"),
					Namespace: gatewayv1beta1.Namespace(nsB.Name),
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: gatewayv1beta1.Group(""),
					Kind:  gatewayv1beta1.Kind("Secret"),
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, refGrant)).To(Succeed())
	t.Cleanup(func() { testClient.Delete(testCtx, refGrant) })

	// Trigger reconciliation by touching an annotation (the controller watches
	// annotation changes via AnnotationChangedPredicate).
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations["test"] = "trigger"
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify the Gateway becomes Accepted and Programmed
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_Infrastructure(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-infra", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-infra",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				Labels: map[gatewayv1.LabelKey]gatewayv1.LabelValue{
					"infra-label": "infra-label-value",
				},
				Annotations: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{
					"infra-annotation": "infra-annotation-value",
				},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Deployment has infrastructure labels/annotations
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())

	// Deployment ObjectMeta
	g.Expect(deploy.Labels).To(HaveKeyWithValue("infra-label", "infra-label-value"))
	g.Expect(deploy.Annotations).To(HaveKeyWithValue("infra-annotation", "infra-annotation-value"))

	// PodTemplate ObjectMeta
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("infra-label", "infra-label-value"))
	g.Expect(deploy.Spec.Template.Annotations).To(HaveKeyWithValue("infra-annotation", "infra-annotation-value"))

	// Selector must NOT include infrastructure labels
	g.Expect(deploy.Spec.Selector.MatchLabels).NotTo(HaveKey("infra-label"))

	// Verify Secret has infrastructure labels/annotations
	var tokenSecret corev1.Secret
	secretKey := client.ObjectKey{Name: "cloudflared-token-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())
	g.Expect(tokenSecret.Labels).To(HaveKeyWithValue("infra-label", "infra-label-value"))
	g.Expect(tokenSecret.Annotations).To(HaveKeyWithValue("infra-annotation", "infra-annotation-value"))
}

func TestGatewayReconciler_InfrastructureParametersRef(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the GatewayClass secret lives
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// Namespace B: where the Gateway and its local secret live
	nsB := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-infra-ref", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Create a local secret in namespace B for the Gateway to reference
	localSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "local-creds",
			Namespace: nsB.Name,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN":  []byte("local-api-token"),
			"CLOUDFLARE_ACCOUNT_ID": []byte("local-account-id"),
		},
	}
	g.Expect(testClient.Create(testCtx, localSecret)).To(Succeed())

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-infra-ref",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "",
					Kind:  "Secret",
					Name:  "local-creds",
				},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the Gateway is accepted and programmed using the local credentials
	// (no ReferenceGrant needed for local ref).
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())

		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))

		// The Cloudflare client must have been constructed with the local credentials
		// from infrastructure.parametersRef, not the GatewayClass credentials.
		g.Expect(testMock.lastClientConfig.APIToken).To(Equal("local-api-token"))
		g.Expect(testMock.lastClientConfig.AccountID).To(Equal("local-account-id"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DNSReconciliation(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Reset mock tracking
	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify EnsureDNSCNAME was called by the Gateway controller
	g.Eventually(func() int {
		return len(testMock.ensureDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 1))
	g.Expect(testMock.ensureDNSCalls[0].Hostname).To(Equal("app.example.com"))
	g.Expect(testMock.ensureDNSCalls[0].Target).To(Equal(cloudflare.TunnelTarget("test-tunnel-id")))

	// Verify DNSRecordsApplied and Ready conditions on HTTPRoute status.parents
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dns.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))

		ready := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DNSStaleCleanup(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-stale", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Pre-populate the mock with a stale DNS record
	testMock.listDNSCNAMEsByTarget = []string{"stale.example.com"}
	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-stale",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.listDNSCNAMEsByTarget = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the stale record is deleted (no HTTPRoutes attached, so stale.example.com is stale)
	g.Eventually(func() int {
		return len(testMock.deleteDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 1))
	g.Expect(testMock.deleteDNSCalls[0].Hostname).To(Equal("stale.example.com"))
}

func TestGatewayReconciler_DNSSkippedHostnames(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-skip", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-skip",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Reset mock tracking after Gateway is programmed
	testMock.ensureDNSCalls = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-skip",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.other.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify DNSRecordsApplied condition on HTTPRoute status.parents shows skipped hostname
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dns.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))
		g.Expect(dns.Message).To(ContainSubstring("Applied hostnames:\n(none)"))
		g.Expect(dns.Message).To(ContainSubstring("Skipped hostnames (not in zone):\n- app.other.com"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify EnsureDNSCNAME was NOT called for the skipped hostname
	g.Consistently(func() bool {
		for _, call := range testMock.ensureDNSCalls {
			if call.Hostname == "app.other.com" {
				return true
			}
		}
		return false
	}).WithTimeout(2 * time.Second).WithPolling(100 * time.Millisecond).Should(BeFalse())
}

func TestGatewayReconciler_HTTPRouteAccepted(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-httproute-accepted", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-httproute", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Reset mock tracking before HTTPRoute creation
	testMock.lastTunnelConfigID = ""
	testMock.lastTunnelConfigIngress = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-httproute",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: gatewayv1.ObjectName(gw.Name),
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-service",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify HTTPRoute becomes Accepted and Ready
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		resolvedRefs := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolvedRefs).NotTo(BeNil())
		g.Expect(resolvedRefs.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify tunnel configuration was updated by the Gateway controller
	// (triggered by HTTPRoute watch).
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigID).To(Equal("test-tunnel-id"))
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(2))
		g.Expect(testMock.lastTunnelConfigIngress[0].Hostname).To(Equal("app.example.com"))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal("http://my-service." + ns.Name + ".svc.cluster.local:8080"))
		g.Expect(testMock.lastTunnelConfigIngress[1].Hostname).To(BeEmpty())
		g.Expect(testMock.lastTunnelConfigIngress[1].Service).To(Equal("http_status:404"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Gateway listener has AttachedRoutes=1
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(1)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceAllowAll(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the Gateway lives (credentials are local)
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// Namespace B: where the HTTPRoute lives
	nsB := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-route-all", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with allowedRoutes.namespaces.from=All
	fromAll := gatewayv1.NamespacesFromAll
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-cross-route-all",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromAll,
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create an HTTPRoute in namespace B referencing the Gateway in namespace A
	nsAGW := gatewayv1.Namespace(nsA.Name)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cross-route",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      gatewayv1.ObjectName(gw.Name),
						Namespace: &nsAGW,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"cross.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-service",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the cross-namespace HTTPRoute is Accepted
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify the Gateway listener counts the cross-namespace route
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(1)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceDenied(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the Gateway lives (default from=Same)
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// Namespace B: where the HTTPRoute lives
	nsB := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-route-denied", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with default listeners (from=Same)
	gw := createTestGateway(g, "test-gw-cross-route-denied", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create an HTTPRoute in namespace B referencing the Gateway in namespace A
	nsAGW := gatewayv1.Namespace(nsA.Name)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cross-route-denied",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      gatewayv1.ObjectName(gw.Name),
						Namespace: &nsAGW,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"denied.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-service",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the cross-namespace HTTPRoute is denied: Accepted=False/NotAllowedByListeners
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonNotAllowedByListeners)))
		g.Expect(accepted.Message).To(ContainSubstring("not allowed by any listener"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify the denied route is NOT counted as attached
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(0)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceSelector(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the Gateway lives
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// Namespace B: where the HTTPRoute lives (will have matching label)
	nsB := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
			Labels: map[string]string{
				"gateway-access": "allowed",
			},
		},
	}
	g.Expect(testClient.Create(testCtx, nsB)).To(Succeed())
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-route-sel", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with allowedRoutes.namespaces.from=Selector
	fromSelector := gatewayv1.NamespacesFromSelector
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-cross-route-sel",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"gateway-access": "allowed",
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create an HTTPRoute in namespace B (which has the matching label)
	nsBGW := gatewayv1.Namespace(nsA.Name)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cross-route-sel",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      gatewayv1.ObjectName(gw.Name),
						Namespace: &nsBGW,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"selector.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-service",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the cross-namespace HTTPRoute is Accepted (selector matches)
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteDeletion(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-httproute-delete", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-httproute-delete", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-httproute-delete",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: gatewayv1.ObjectName(gw.Name),
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"delete.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-service",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())

	// Wait for HTTPRoute to be Accepted
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Reset mock tracking
	testMock.lastTunnelConfigIngress = nil

	// Delete the HTTPRoute
	var latest gatewayv1.HTTPRoute
	g.Expect(testClient.Get(testCtx, routeKey, &latest)).To(Succeed())
	g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())

	// Wait for HTTPRoute to be fully deleted
	g.Eventually(func() error {
		return testClient.Get(testCtx, routeKey, &latest)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Satisfy(apierrors.IsNotFound))

	// Verify tunnel config was rebuilt by the Gateway controller (only catch-all remains)
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(1))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal("http_status:404"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GatewayClassNotReadyThenRecovers(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	// Create a GatewayClass pointing to a nonexistent secret so it becomes not ready.
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gw-class-notready-recovers",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "cloudflare-creds",
				Namespace: func() *gatewayv1.Namespace { ns := gatewayv1.Namespace(ns.Name); return &ns }(),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for GatewayClass to become not ready (secret doesn't exist yet).
	gcKey := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, gcKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create Gateway and HTTPRoute while GatewayClass is not ready.
	gw := createTestGateway(g, "test-gw-notready-recovers", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-httproute-notready-recovers",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: gatewayv1.ObjectName(gw.Name),
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"notready.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-service",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Gateway should not be reconciled while GatewayClass is not ready.
	// Conditions stay at webhook defaults (Unknown/Pending).
	gwKey := client.ObjectKeyFromObject(gw)
	routeKey := client.ObjectKeyFromObject(route)
	g.Consistently(func(g Gomega) {
		var gwResult gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &gwResult)).To(Succeed())
		for _, c := range gwResult.Status.Conditions {
			g.Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			g.Expect(c.Reason).To(Equal("Pending"))
		}

		var routeResult gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &routeResult)).To(Succeed())
		g.Expect(routeResult.Status.Parents).To(BeEmpty())
	}).WithTimeout(3 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Now create the secret so the GatewayClass becomes ready.
	createTestSecret(g, ns.Name)

	// The GatewayClass watch should trigger Gateway reconciliation.
	// Gateway should become Programmed and HTTPRoute should get parent status.
	waitForGatewayClassReady(g, gc)
	waitForGatewayProgrammed(g, gw)

	g.Eventually(func(g Gomega) {
		var routeResult gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &routeResult)).To(Succeed())
		g.Expect(routeResult.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(routeResult.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeploymentPatches(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-deploy-patches", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-deploy-patches",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationDeploymentPatches: `
- op: add
  path: /spec/template/spec/tolerations
  value:
    - key: "example.com/special-node"
      operator: "Exists"
`,
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify Deployment has tolerations from the patch
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Tolerations[0].Key).To(Equal("example.com/special-node"))
	g.Expect(deploy.Spec.Template.Spec.Tolerations[0].Operator).To(Equal(corev1.TolerationOperator("Exists")))
}

func TestGatewayReconciler_DeploymentPatchesMultiple(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-multi", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-multi",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationDeploymentPatches: `
- op: add
  path: /spec/replicas
  value: 3
- op: add
  path: /spec/template/spec/tolerations
  value:
    - key: "node-role"
      operator: "Exists"
`,
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify both replicas and tolerations are applied
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(3)))
	g.Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Tolerations[0].Key).To(Equal("node-role"))
}

func TestGatewayReconciler_DeploymentPatchesResourceRequests(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-resources", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-resources",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationDeploymentPatches: `
- op: add
  path: /spec/template/spec/containers/0/resources
  value:
    requests:
      memory: "128Mi"
      cpu: "250m"
    limits:
      memory: "256Mi"
`,
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify Deployment container has resource requests/limits
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	resources := deploy.Spec.Template.Spec.Containers[0].Resources
	g.Expect(resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(resources.Requests.Cpu().String()).To(Equal("250m"))
	g.Expect(resources.Limits.Memory().String()).To(Equal("256Mi"))
}

func TestGatewayReconciler_DeploymentPatchesInvalidYAML(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-invalid-yaml", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-invalid-yaml",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationDeploymentPatches: `not: valid: yaml: [`,
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// The Gateway should report a terminal error via Ready=False condition
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationFailed))
		g.Expect(ready.Message).To(ContainSubstring("deploymentPatches"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeploymentPatchesInvalidOps(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-invalid-ops", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-invalid-ops",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationDeploymentPatches: `
- op: replace
  path: /spec/nonexistent/field
  value: "something"
`,
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// The Gateway should report a terminal error via Ready=False condition
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationFailed))
		g.Expect(ready.Message).To(ContainSubstring("deploymentPatches"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeletionReconcileDisabled(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-disabled", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Reset deleteCalled tracking.
	testMock.deleteCalled = false
	testMock.deletedID = ""

	gw := createTestGateway(g, "test-gw-del-disabled", ns.Name, gc.Name)

	// Wait for Gateway to be Programmed (so Deployment + Secret exist).
	gwKey := client.ObjectKeyFromObject(gw)
	waitForGatewayProgrammed(g, gw)

	// Verify Deployment and Secret exist.
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	secretKey := client.ObjectKey{Name: "cloudflared-token-" + gw.Name, Namespace: gw.Namespace}
	var deploy appsv1.Deployment
	var tokenSecret corev1.Secret
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())

	// Add reconcile=disabled annotation, then delete the Gateway.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations[apiv1.AnnotationReconcile] = apiv1.ValueDisabled
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait for Gateway to be fully deleted.
	g.Eventually(func() error {
		var latest gatewayv1.Gateway
		return testClient.Get(testCtx, gwKey, &latest)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Satisfy(apierrors.IsNotFound))

	// Deployment and Secret should STILL EXIST (owner ref removed, not GC'd).
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())

	// Owner references should be removed.
	g.Expect(deploy.OwnerReferences).To(BeEmpty())
	g.Expect(tokenSecret.OwnerReferences).To(BeEmpty())

	// Mock DeleteTunnel should NOT have been called (disabled finalization skips tunnel cleanup).
	g.Expect(testMock.deleteCalled).To(BeFalse())

	// GatewayClass finalizer should be removed.
	g.Eventually(func() []string {
		var gcResult gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &gcResult); err != nil {
			return []string{err.Error()}
		}
		return gcResult.Finalizers
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).ShouldNot(ContainElement(apiv1.FinalizerGatewayClass(gw)))

	// Verify ReconciliationDisabled events were emitted for Deployment and Secret.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, deploy.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationDisabled, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("removed owner reference"))
		g.Expect(e.Related).NotTo(BeNil(), "related should reference the Gateway")
		g.Expect(e.Related.Name).To(Equal(gw.Name))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, tokenSecret.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationDisabled, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("removed owner reference"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Clean up orphaned resources.
	testClient.Delete(testCtx, &deploy)
	testClient.Delete(testCtx, &tokenSecret)
}

func TestGatewayReconciler_DeletionWithHTTPRoutes(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-routes", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-del-routes",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())

	waitForGatewayProgrammed(g, gw)
	gwKey := client.ObjectKeyFromObject(gw)

	// Reset mock tracking.
	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.deleteCalled = false

	// Create HTTPRoute.
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-del",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"del.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())

	// Wait for DNS condition to be True on the HTTPRoute.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Setup for cleanup: set zone IDs and stale DNS records for finalization cleanup.
	testMock.zoneIDs = []string{"zone-1"}
	testMock.listDNSCNAMEsByTarget = []string{"del.example.com"}
	testMock.deleteDNSCalls = nil

	// Delete the Gateway.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait for Gateway to be fully deleted (finalization involves multiple
	// steps: delete deployment, wait for it, cleanup DNS, delete tunnel, etc.).
	g.Eventually(func() error {
		var latest gatewayv1.Gateway
		return testClient.Get(testCtx, gwKey, &latest)
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Satisfy(apierrors.IsNotFound))

	// Tunnel should have been deleted.
	g.Expect(testMock.deleteCalled).To(BeTrue())

	// DNS cleanup should have been called.
	g.Expect(testMock.deleteDNSCalls).NotTo(BeEmpty())
	g.Expect(testMock.deleteDNSCalls[0].Hostname).To(Equal("del.example.com"))

	// HTTPRoute status.parents should be empty (our Gateway's entry removed).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		for _, s := range result.Status.Parents {
			if s.ControllerName == apiv1.ControllerName {
				g.Expect(s.ParentRef.Name).NotTo(Equal(gatewayv1.ObjectName(gw.Name)),
					"Expected Gateway's status.parents entry to be removed")
			}
		}
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// GatewayClass finalizer should be removed.
	g.Eventually(func() []string {
		var gcResult gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &gcResult); err != nil {
			return []string{err.Error()}
		}
		return gcResult.Finalizers
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).ShouldNot(ContainElement(apiv1.FinalizerGatewayClass(gw)))

	// Verify "Gateway finalized" event.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(Equal("Gateway finalized"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Verify "Removed status entry" event on the HTTPRoute.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, route.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, apiv1.EventActionReconcile, "Removed status entry")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Cleanup.
	testMock.zoneIDs = nil
	testMock.listDNSCNAMEsByTarget = nil
	testClient.Delete(testCtx, route)
}

func TestGatewayReconciler_StaleRouteParentRefRemoved(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-stale-ref", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-stale-ref", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-stale",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"stale.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for the route to be Accepted.
	routeKey := client.ObjectKeyFromObject(route)
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Update HTTPRoute to remove the parentRef (point to a nonexistent gateway).
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &latest)).To(Succeed())
		latest.Spec.ParentRefs = []gatewayv1.ParentReference{
			{Name: "nonexistent-gateway"},
		}
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// HTTPRoute status.parents for the old Gateway should be removed.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		for _, s := range result.Status.Parents {
			if s.ControllerName == apiv1.ControllerName &&
				s.ParentRef.Namespace != nil && string(*s.ParentRef.Namespace) == gw.Namespace &&
				string(s.ParentRef.Name) == gw.Name {
				g.Expect("stale entry").To(Equal("removed"), "Stale status.parents entry should be removed")
			}
		}
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Gateway listener AttachedRoutes should drop to 0.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(0)))
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Tunnel ingress should be catch-all only.
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(1))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal("http_status:404"))
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceBackendDenied(t *testing.T) {
	g := NewWithT(t)

	// ns-A: where the Gateway and HTTPRoute live.
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// ns-B: where the Service backend lives (cross-namespace backendRef).
	nsB := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-backend-denied", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-backend-denied", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// HTTPRoute in nsA references a Service in nsB (cross-namespace backendRef) with no ReferenceGrant.
	nsBNS := gatewayv1.Namespace(nsB.Name)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-backend-denied",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"backend-denied.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name:      "cross-service",
								Port:      new(gatewayv1.PortNumber(8080)),
								Namespace: &nsBNS,
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify: Accepted=True, ResolvedRefs=False/RefNotPermitted.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))

		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		resolvedRefs := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolvedRefs).NotTo(BeNil())
		g.Expect(resolvedRefs.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(resolvedRefs.Reason).To(Equal(string(gatewayv1.RouteReasonRefNotPermitted)))
		g.Expect(resolvedRefs.Message).To(ContainSubstring("cross-service"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Tunnel ingress should have only catch-all (denied backend excluded).
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(1))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal("http_status:404"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceBackendGranted(t *testing.T) {
	g := NewWithT(t)

	// ns-A: where the Gateway and HTTPRoute live.
	nsA := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsA) })

	// ns-B: where the Service backend lives.
	nsB := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-backend-granted", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-backend-granted", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create HTTPRoute with cross-namespace backendRef (no grant yet).
	nsBNS := gatewayv1.Namespace(nsB.Name)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-backend-granted",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"backend-granted.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name:      "cross-service",
								Port:      new(gatewayv1.PortNumber(8080)),
								Namespace: &nsBNS,
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for ResolvedRefs=False (no grant yet).
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		resolvedRefs := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolvedRefs).NotTo(BeNil())
		g.Expect(resolvedRefs.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create ReferenceGrant in nsB allowing HTTPRoutes from nsA to reference Services.
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-backend-ref",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1beta1.Group(gatewayv1.GroupName),
					Kind:      gatewayv1beta1.Kind("HTTPRoute"),
					Namespace: gatewayv1beta1.Namespace(nsA.Name),
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: gatewayv1beta1.Group(""),
					Kind:  gatewayv1beta1.Kind("Service"),
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, refGrant)).To(Succeed())
	t.Cleanup(func() { testClient.Delete(testCtx, refGrant) })

	// Trigger re-reconciliation via annotation.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations["test"] = "trigger-grant"
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait for ResolvedRefs=True.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		resolvedRefs := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolvedRefs).NotTo(BeNil())
		g.Expect(resolvedRefs.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Tunnel ingress should contain the cross-namespace service URL.
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(2))
		g.Expect(testMock.lastTunnelConfigIngress[0].Hostname).To(Equal("backend-granted.example.com"))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal(
			"http://cross-service." + nsB.Name + ".svc.cluster.local:8080"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DNSZoneRemovalCleanup(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-removal", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-removal",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	testMock.ensureDNSCalls = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-removal",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"removal.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for DNS condition True.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Setup mock for cleanup: simulate existing DNS records.
	testMock.zoneIDs = []string{"zone-1"}
	testMock.listDNSCNAMEsByTarget = []string{"removal.example.com"}
	testMock.deleteDNSCalls = nil

	// Remove zoneName annotation from Gateway.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		delete(latest.Annotations, apiv1.AnnotationZoneName)
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// DNS cleanup should be called.
	g.Eventually(func() int {
		return len(testMock.deleteDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 1))
	g.Expect(testMock.deleteDNSCalls[0].Hostname).To(Equal("removal.example.com"))

	// HTTPRoute DNS condition should be removed.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).To(BeNil(), "DNSRecordsApplied condition should be removed when DNS is disabled")
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Cleanup.
	testMock.zoneIDs = nil
	testMock.listDNSCNAMEsByTarget = nil
}

func TestGatewayReconciler_DeploymentProgressDeadlineExceeded(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-deadline", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-deadline", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed (background goroutine sets Available=True).
	waitForGatewayProgrammed(g, gw)

	// Now simulate a progress deadline exceeded by patching the Deployment's
	// Progressing condition to False. The background goroutine skips Deployments
	// where ReadyReplicas == desired, so after Programmed we can safely
	// patch only the Progressing condition.
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Eventually(func(g Gomega) {
		var deploy appsv1.Deployment
		g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
		for i, c := range deploy.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing {
				deploy.Status.Conditions[i].Status = "False"
				deploy.Status.Conditions[i].Reason = "ProgressDeadlineExceeded"
			}
		}
		g.Expect(testClient.Status().Update(testCtx, &deploy)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Trigger re-reconciliation via annotation change.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations["test"] = "trigger-deadline"
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Assert: Ready=False/ReconciliationFailed, Programmed=False (deadline exceeded
	// takes priority over available in the controller logic).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())

		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationFailed))
		g.Expect(ready.Message).To(ContainSubstring("exceeded progress deadline"))

		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRoutePathMatches(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-path", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-path", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	testMock.lastTunnelConfigIngress = nil

	pathPrefix := gatewayv1.PathMatchPathPrefix
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-path",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"path.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &pathPrefix,
								Value: new(string),
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	// Set path value after creation since we need a pointer.
	*route.Spec.Rules[0].Matches[0].Path.Value = "/api/v1"

	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify tunnel ingress rule has Path="/api/v1".
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(2))
		g.Expect(testMock.lastTunnelConfigIngress[0].Hostname).To(Equal("path.example.com"))
		g.Expect(testMock.lastTunnelConfigIngress[0].Path).To(Equal("/api/v1"))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal("http://my-service." + ns.Name + ".svc.cluster.local:8080"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteMultipleRulesAndHostnames(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-multi-rules", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-multi-rules", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	testMock.lastTunnelConfigIngress = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-multi",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com", "api.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "frontend", Port: new(gatewayv1.PortNumber(80)),
							},
						}},
					},
				},
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "backend", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// 2 rules x 2 hostnames = 4 hostname rules + 1 catch-all = 5 total.
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(5))
		// Last rule is catch-all.
		g.Expect(testMock.lastTunnelConfigIngress[4].Service).To(Equal("http_status:404"))
		g.Expect(testMock.lastTunnelConfigIngress[4].Hostname).To(BeEmpty())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteNoBackends(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-no-backends", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-no-backends", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	testMock.lastTunnelConfigIngress = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-no-backends",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"nobackend.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					// Empty BackendRefs.
					BackendRefs: []gatewayv1.HTTPBackendRef{},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Route should be Accepted.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Tunnel ingress should have only catch-all 404.
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.lastTunnelConfigIngress).To(HaveLen(1))
		g.Expect(testMock.lastTunnelConfigIngress[0].Service).To(Equal("http_status:404"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_MultipleListeners(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-multi-listeners", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with HTTP + HTTPS + TCP listeners.
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-multi-listeners",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
				},
				{
					Name:     "tcp",
					Protocol: gatewayv1.TCPProtocolType,
					Port:     8080,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create HTTPRoute targeting only the "http" listener via sectionName.
	httpSection := gatewayv1.SectionName("http")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-multi-listeners",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:        gatewayv1.ObjectName(gw.Name),
						SectionName: &httpSection,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"multi.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify listener statuses.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(3))

		var httpLS, httpsLS, tcpLS *gatewayv1.ListenerStatus
		for i := range result.Status.Listeners {
			switch result.Status.Listeners[i].Name {
			case "http":
				httpLS = &result.Status.Listeners[i]
			case "https":
				httpsLS = &result.Status.Listeners[i]
			case "tcp":
				tcpLS = &result.Status.Listeners[i]
			}
		}

		// HTTP: Accepted=True, AttachedRoutes=1.
		g.Expect(httpLS).NotTo(BeNil())
		httpAccepted := conditions.Find(httpLS.Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(httpAccepted).NotTo(BeNil())
		g.Expect(httpAccepted.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(httpLS.AttachedRoutes).To(Equal(int32(1)))

		// HTTPS: Accepted=True, AttachedRoutes=0.
		g.Expect(httpsLS).NotTo(BeNil())
		httpsAccepted := conditions.Find(httpsLS.Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(httpsAccepted).NotTo(BeNil())
		g.Expect(httpsAccepted.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(httpsLS.AttachedRoutes).To(Equal(int32(0)))

		// TCP: Accepted=False/UnsupportedProtocol, AttachedRoutes=0.
		g.Expect(tcpLS).NotTo(BeNil())
		tcpAccepted := conditions.Find(tcpLS.Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(tcpAccepted).NotTo(BeNil())
		g.Expect(tcpAccepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(tcpAccepted.Reason).To(Equal(string(gatewayv1.ListenerReasonUnsupportedProtocol)))
		g.Expect(tcpLS.AttachedRoutes).To(Equal(int32(0)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GatewayClassNoParametersRef(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	// Create a GatewayClass without parametersRef.
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gw-class-no-params",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			// No ParametersRef.
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// GatewayClass will get Ready=False from the GatewayClass controller (no Secret).
	// We need to manually set Ready=True so the Gateway controller proceeds to readCredentials.
	gcKey := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, gcKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	g.Eventually(func(g Gomega) {
		var latest gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, gcKey, &latest)).To(Succeed())
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Status.Conditions = conditions.Set(latest.Status.Conditions, []metav1.Condition{
			{
				Type:               apiv1.ConditionReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: latest.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             apiv1.ReasonReconciliationSucceeded,
				Message:            "Ready",
			},
		})
		g.Expect(testClient.Status().Patch(testCtx, &latest, patch)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	gw := createTestGateway(g, "test-gw-no-params", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Gateway should get Accepted=False/InvalidParameters, "no parametersRef".
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("no parametersRef"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_InfrastructureParametersRefInvalidKind(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-infra-invalid", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with infrastructure.parametersRef.kind = "ConfigMap" (invalid).
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-infra-invalid",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "",
					Kind:  "ConfigMap",
					Name:  "some-config",
				},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Assert: Accepted=False/InvalidParameters, "must reference a core/v1 Secret".
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("must reference a core/v1 Secret"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GatewayClassSwitching(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)

	// Create two GatewayClasses.
	gcA := createTestGatewayClass(g, "test-gw-class-switch-a", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gcA), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	gcB := createTestGatewayClass(g, "test-gw-class-switch-b", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gcB), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gcA)
	waitForGatewayClassReady(g, gcB)

	// Gateway referencing gc-A.
	gw := createTestGateway(g, "test-gw-switch", ns.Name, gcA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Verify gc-A has the finalizer.
	g.Eventually(func(g Gomega) {
		var gcResult gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gcA), &gcResult)).To(Succeed())
		g.Expect(gcResult.Finalizers).To(ContainElement(apiv1.FinalizerGatewayClass(gw)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Switch Gateway to reference gc-B.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		latest.Spec.GatewayClassName = gatewayv1.ObjectName(gcB.Name)
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// gc-A finalizer should be removed.
	g.Eventually(func(g Gomega) {
		var gcResult gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gcA), &gcResult)).To(Succeed())
		g.Expect(gcResult.Finalizers).NotTo(ContainElement(apiv1.FinalizerGatewayClass(gw)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// gc-B finalizer should be added.
	g.Eventually(func(g Gomega) {
		var gcResult gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gcB), &gcResult)).To(Succeed())
		g.Expect(gcResult.Finalizers).To(ContainElement(apiv1.FinalizerGatewayClass(gw)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DNSMultipleHostnamesMixed(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-mixed", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-mixed",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	testMock.ensureDNSCalls = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-mixed",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com", "api.example.com", "app.other.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for DNS condition with applied and skipped hostnames.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dns.Message).To(ContainSubstring("app.example.com"))
		g.Expect(dns.Message).To(ContainSubstring("api.example.com"))
		g.Expect(dns.Message).To(ContainSubstring("app.other.com"))
		g.Expect(dns.Message).To(ContainSubstring("Skipped hostnames"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify DNS calls were made for the in-zone hostnames only.
	g.Eventually(func() int {
		return len(testMock.ensureDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 2))
	var dnsHostnames []string
	for _, call := range testMock.ensureDNSCalls {
		dnsHostnames = append(dnsHostnames, call.Hostname)
	}
	g.Expect(dnsHostnames).To(ContainElement("app.example.com"))
	g.Expect(dnsHostnames).To(ContainElement("api.example.com"))
	g.Expect(dnsHostnames).NotTo(ContainElement("app.other.com"))
}

func TestGatewayReconciler_ReconcileDisabled(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-disabled", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-disabled", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			// Remove disabled annotation so cleanup can proceed.
			delete(latest.Annotations, apiv1.AnnotationReconcile)
			testClient.Update(testCtx, &latest)
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Add reconcile=disabled annotation.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations[apiv1.AnnotationReconcile] = apiv1.ValueDisabled
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait a bit for any pending reconciliation to complete.
	time.Sleep(1 * time.Second)

	// Reset tunnel config tracking.
	testMock.lastTunnelConfigID = ""
	prevIngress := testMock.lastTunnelConfigIngress

	// Trigger re-reconciliation via another annotation change.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations["test"] = "trigger-disabled"
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Tunnel config should NOT be updated (reconciliation was skipped).
	g.Consistently(func() string {
		return testMock.lastTunnelConfigID
	}).WithTimeout(3 * time.Second).WithPolling(100 * time.Millisecond).Should(BeEmpty())

	// Ingress should remain unchanged.
	g.Expect(testMock.lastTunnelConfigIngress).To(Equal(prevIngress))
}

func TestGatewayReconciler_CloudflareTunnelLookupError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-tunnel-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Set error before Gateway creation.
	testMock.getTunnelIDByNameErr = fmt.Errorf("cloudflare API unavailable")

	gw := createTestGateway(g, "test-gw-tunnel-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.getTunnelIDByNameErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Assert: Ready=Unknown/ProgressingWithRetry, message contains "looking up tunnel".
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("looking up tunnel"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning/ProgressingWithRetry/Reconcile event.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionReconcile, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("Reconciliation failed"))
		g.Expect(e.Note).To(ContainSubstring("looking up tunnel"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareTunnelTokenError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-token-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.getTunnelTokenErr = fmt.Errorf("token retrieval failed")

	gw := createTestGateway(g, "test-gw-token-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.getTunnelTokenErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("tunnel token"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareUpdateConfigError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-config-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// First create Gateway without error to get it programmed.
	gw := createTestGateway(g, "test-gw-config-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.updateTunnelConfigErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Now inject error and create HTTPRoute (triggers config update).
	testMock.updateTunnelConfigErr = fmt.Errorf("config update failed")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-config-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"config-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("tunnel configuration"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareDNSFindZoneError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-zone-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-zone-err",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.findZoneIDErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Set findZoneID error and create HTTPRoute.
	testMock.findZoneIDErr = fmt.Errorf("zone lookup failed")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-zone-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"zone-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// HTTPRoute should get Ready=Unknown/ProgressingWithRetry (DNS error propagates).
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		ready := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("zone"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareDNSEnsureError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-ensure-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-ensure-err",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.ensureDNSErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Set ensureDNS error.
	testMock.ensureDNSErr = fmt.Errorf("DNS ensure failed")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-ensure-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"ensure-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// HTTPRoute should get Ready=Unknown/ProgressingWithRetry.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		ready := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareDeleteTunnelErrorDuringFinalization(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-tunnel-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-del-tunnel-err", ns.Name, gc.Name)

	waitForGatewayProgrammed(g, gw)
	gwKey := client.ObjectKeyFromObject(gw)

	// Set deleteTunnel error.
	testMock.deleteTunnelErr = fmt.Errorf("tunnel deletion failed")

	// Delete the Gateway.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Gateway should NOT be deleted (finalizer stuck).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("deleting tunnel"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning/ProgressingWithRetry/Finalize event.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("Finalization failed"))
		g.Expect(e.Note).To(ContainSubstring("deleting tunnel"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Clear error so cleanup (resetMockErrors) doesn't leave a stuck Gateway.
	// The controller will eventually retry finalization via backoff.
	testMock.deleteTunnelErr = nil
}

func TestGatewayReconciler_CloudflareListZoneIDsErrorDuringFinalization(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-list-zones-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-list-zones-err",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())

	waitForGatewayProgrammed(g, gw)
	gwKey := client.ObjectKeyFromObject(gw)

	// Create a route so DNS is "previously enabled".
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-list-zones-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"zones-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for DNS condition to be True.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Set listZoneIDs error and delete Gateway.
	testMock.listZoneIDsErr = fmt.Errorf("zone listing failed")

	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Gateway should be stuck (finalizer).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("cleaning up DNS during finalization"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Clear error so cleanup (resetMockErrors) doesn't leave a stuck Gateway.
	// The controller will eventually retry finalization via backoff.
	testMock.listZoneIDsErr = nil
}

func TestGatewayReconciler_CloudflareCreateTunnelError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-create-tunnel-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Make GetTunnelIDByName return empty (no existing tunnel) so ensureTunnel
	// calls CreateTunnel, which we make fail.
	savedTunnelID := testMock.tunnelID
	testMock.tunnelID = ""
	testMock.createTunnelErr = fmt.Errorf("tunnel creation failed")

	gw := createTestGateway(g, "test-gw-create-tunnel-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.tunnelID = savedTunnelID
		testMock.createTunnelErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("creating tunnel"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareGetTunnelConfigError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-get-config-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-get-config-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.getTunnelConfigurationErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Inject error and create HTTPRoute to trigger reconcileTunnelIngress.
	testMock.getTunnelConfigurationErr = fmt.Errorf("config fetch failed")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-get-config-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"get-config-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("getting tunnel configuration"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareListDNSCNAMEsError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-list-cnames-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-list-cnames-err",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.listDNSCNAMEsByTargetErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Inject error and create HTTPRoute.
	testMock.listDNSCNAMEsByTargetErr = fmt.Errorf("list CNAMEs failed")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-list-cnames-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"list-cnames-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Non-fatal DNS error: Gateway gets a Warning event, HTTPRoute gets error in condition.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		ready := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning event on Gateway.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionReconcile, "list DNS CNAMEs")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Verify Gateway is still Programmed (non-fatal DNS error doesn't break reconcile).
	g.Consistently(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(2 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareDeleteDNSCNAMEError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-cname-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Pre-populate the mock with a stale DNS record and set delete error.
	testMock.listDNSCNAMEsByTarget = []string{"stale-del-err.example.com"}
	testMock.deleteDNSErr = fmt.Errorf("DNS delete failed")

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-del-cname-err",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.listDNSCNAMEsByTarget = nil
		testMock.deleteDNSErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Verify Warning event on Gateway with the DNS delete error (non-fatal).
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionReconcile, "delete stale DNS CNAME")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareClientFactoryError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-factory-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.newClientErr = fmt.Errorf("client creation failed")

	gw := createTestGateway(g, "test-gw-factory-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.newClientErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("creating cloudflare client"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareDNSCleanupOnZoneRemovalError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-zone-rm-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-zone-rm-err",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationZoneName: "example.com",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.listZoneIDsErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create HTTPRoute with in-zone hostname.
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-zone-rm-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"zone-rm-err.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
							},
						}},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for DNS condition True on HTTPRoute.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Set ListZoneIDs error and remove zone annotation.
	testMock.listZoneIDsErr = fmt.Errorf("zone listing failed")

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		delete(latest.Annotations, apiv1.AnnotationZoneName)
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning event on Gateway with DNS cleanup error (non-fatal).
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionReconcile, "clean up DNS records")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareCleanupConnectionsErrorDuringFinalization(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-conn-cleanup-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-conn-cleanup-err", ns.Name, gc.Name)

	waitForGatewayProgrammed(g, gw)
	gwKey := client.ObjectKeyFromObject(gw)

	// Set cleanupTunnelConnections error.
	testMock.cleanupTunnelConnectionsErr = fmt.Errorf("connections cleanup failed")

	// Delete the Gateway.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Gateway should NOT be deleted (finalizer stuck).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("cleaning up tunnel connections"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning/ProgressingWithRetry/Finalize event.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("Finalization failed"))
		g.Expect(e.Note).To(ContainSubstring("cleaning up tunnel connections"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Clear error so cleanup (resetMockErrors) doesn't leave a stuck Gateway.
	testMock.cleanupTunnelConnectionsErr = nil
}

func TestGatewayReconciler_CloudflareTunnelLookupErrorDuringFinalization(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-fin-lookup-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-fin-lookup-err", ns.Name, gc.Name)

	waitForGatewayProgrammed(g, gw)
	gwKey := client.ObjectKeyFromObject(gw)

	// Set getTunnelIDByName error (will hit during finalization).
	testMock.getTunnelIDByNameErr = fmt.Errorf("tunnel lookup failed")

	// Delete the Gateway.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Gateway should NOT be deleted (finalizer stuck).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("looking up tunnel for deletion"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning/ProgressingWithRetry/Finalize event.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("Finalization failed"))
		g.Expect(e.Note).To(ContainSubstring("looking up tunnel for deletion"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Clear error so cleanup (resetMockErrors) doesn't leave a stuck Gateway.
	testMock.getTunnelIDByNameErr = nil
}
