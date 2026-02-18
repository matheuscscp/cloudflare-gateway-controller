// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
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

	// Verify the Gateway is accepted (no ReferenceGrant needed for local ref)
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

	// Verify DNSManagement condition on HTTPRoute status.parents
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dns.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))
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

	// Verify HTTPRoute becomes Accepted
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

func TestGatewayReconciler_HTTPRouteGatewayNotReady(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { testClient.Delete(testCtx, ns) })

	// Create a GatewayClass with a missing secret so the Gateway never becomes programmed
	// (and thus has no tunnel ID).
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gw-class-httproute-notready",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "nonexistent-secret",
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

	gw := createTestGateway(g, "test-gw-httproute-notready", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be not accepted (due to missing secret)
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-httproute-notready",
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

	// HTTPRoute should NOT get a parent status because the Gateway controller
	// fails at credential check and never reaches the route status update.
	routeKey := client.ObjectKeyFromObject(route)
	g.Consistently(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(BeEmpty())
	}).WithTimeout(3 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
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

	// The Gateway should report an error via Ready condition
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
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

	// The Gateway should report an error via Ready condition
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Message).To(ContainSubstring("deploymentPatches"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}
