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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-happy", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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

		// Addresses
		g.Expect(result.Status.Addresses).To(HaveLen(1))
		g.Expect(result.Status.Addresses[0].Value).To(Equal(cloudflare.TunnelTarget("test-tunnel-id")))
		g.Expect(result.Status.Addresses[0].Type).NotTo(BeNil())
		g.Expect(*result.Status.Addresses[0].Type).To(Equal(gatewayv1.HostnameAddressType))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify tunnel token Secret created
	var tokenSecret corev1.Secret
	secretKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())
	g.Expect(tokenSecret.Data).To(HaveKey("TUNNEL_TOKEN"))

	// Verify Deployment created
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))
	g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(controller.DefaultCloudflaredImage))
	g.Expect(deploy.Spec.Template.Spec.Containers[1].Name).To(Equal("sidecar"))
	g.Expect(deploy.Spec.Template.Spec.Containers[1].Image).To(Equal("test-sidecar-image:latest"))

	// Verify env uses secretKeyRef, not plain Value
	env := deploy.Spec.Template.Spec.Containers[0].Env
	g.Expect(env).To(HaveLen(1))
	g.Expect(env[0].Name).To(Equal("TUNNEL_TOKEN"))
	g.Expect(env[0].Value).To(BeEmpty())
	g.Expect(env[0].ValueFrom).NotTo(BeNil())
	g.Expect(env[0].ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal("gateway-" + gw.Name))
	g.Expect(env[0].ValueFrom.SecretKeyRef.Key).To(Equal("TUNNEL_TOKEN"))

	// Verify default resource limits are set on cloudflared container
	resources := deploy.Spec.Template.Spec.Containers[0].Resources
	g.Expect(resources.Requests.Cpu().String()).To(Equal("50m"))
	g.Expect(resources.Requests.Memory().String()).To(Equal("64Mi"))
	g.Expect(resources.Limits.Cpu().String()).To(Equal("500m"))
	g.Expect(resources.Limits.Memory().String()).To(Equal("256Mi"))

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

	// Verify CloudflareGatewayStatus (CGS) was created for this Gateway.
	var cgs apiv1.CloudflareGatewayStatus
	cgsKey := client.ObjectKey{Name: gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, cgsKey, &cgs)).To(Succeed())

	// Owner reference points to the Gateway.
	g.Expect(cgs.OwnerReferences).To(HaveLen(1))
	g.Expect(cgs.OwnerReferences[0].Name).To(Equal(gw.Name))
	g.Expect(cgs.OwnerReferences[0].Kind).To(Equal(apiv1.KindGateway))

	// Finalizer prevents accidental deletion.
	g.Expect(cgs.Finalizers).To(ContainElement(apiv1.Finalizer))

	// Tunnel status.
	g.Expect(cgs.Status.Tunnel).NotTo(BeNil())
	g.Expect(cgs.Status.Tunnel.ID).To(Equal("test-tunnel-id"))

	// Inventory lists managed Kubernetes objects (sidecar enabled in tests).
	g.Expect(cgs.Status.Inventory).To(ConsistOf(
		apiv1.ResourceRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: "v1", Kind: "Secret", Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: "v1", Kind: "ConfigMap", Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: "v1", Kind: "ServiceAccount", Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role", Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding", Name: "gateway-" + gw.Name},
	))

	// CGS conditions mirror Gateway conditions.
	cgsReady := conditions.Find(cgs.Status.Conditions, apiv1.ConditionReady)
	g.Expect(cgsReady).NotTo(BeNil())
	g.Expect(cgsReady.Status).To(Equal(metav1.ConditionTrue))
}

func TestGatewayReconciler_UnsupportedProtocol(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-unsupported", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonListenersNotValid)))
		g.Expect(accepted.Message).To(ContainSubstring("not supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_UnsupportedAddress(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-addr", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-addr",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Addresses: []gatewayv1.GatewaySpecAddress{
				{Value: "1.2.3.4"},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonUnsupportedAddress)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_ListenerTLS(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-tls", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	tlsMode := gatewayv1.TLSModeTerminate
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-tls",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "fake-cert"},
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonListenersNotValid)))
		g.Expect(accepted.Message).To(ContainSubstring("tls is not supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_ListenerHostname(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-hostname", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	hostname := gatewayv1.Hostname("example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-hostname",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
					Hostname: &hostname,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonListenersNotValid)))
		g.Expect(accepted.Message).To(ContainSubstring("hostname is not supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_AllowedRoutesKinds(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-kinds", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-kinds",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Kinds: []gatewayv1.RouteGroupKind{
							{Kind: "TCPRoute"},
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonListenersNotValid)))
		g.Expect(accepted.Message).To(ContainSubstring("Only HTTPRoute kind is supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_InvalidReconcileEveryAnnotation(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-interval", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-interval",
			Namespace: ns.Name,
			Annotations: map[string]string{
				apiv1.AnnotationReconcileEvery: "not-a-duration",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("invalid duration"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_Deletion(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-deletion", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(e.Note).To(HavePrefix("Gateway finalized"))
		g.Expect(e.Note).To(ContainSubstring("deleted cloudflared Deployment"))
		g.Expect(e.Note).To(ContainSubstring("deleted tunnel"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CrossNamespaceSecret(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the Secret lives
	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	// Namespace B: where the Gateway lives
	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-ns", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, refGrant) })

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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-infra", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
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
	secretKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())
	g.Expect(tokenSecret.Labels).To(HaveKeyWithValue("infra-label", "infra-label-value"))
	g.Expect(tokenSecret.Annotations).To(HaveKeyWithValue("infra-annotation", "infra-annotation-value"))
}

func TestGatewayReconciler_InfrastructureParametersRef(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the GatewayClass secret lives
	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	// Namespace B: where the Gateway and its local secret live
	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-infra-ref", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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

func TestGatewayReconciler_GatewayClassNotReadyThenRecovers(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

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
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-deploy-patches", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-deploy-patches-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Deployment: &apiv1.DeploymentConfig{
				Patches: []apiv1.JSONPatchOperation{
					{
						Op:    "add",
						Path:  "/spec/template/spec/tolerations",
						Value: &apiextensionsv1.JSON{Raw: []byte(`[{"key":"example.com/special-node","operator":"Exists"}]`)},
					},
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-deploy-patches",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify Deployment has tolerations from the patch
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Tolerations[0].Key).To(Equal("example.com/special-node"))
	g.Expect(deploy.Spec.Template.Spec.Tolerations[0].Operator).To(Equal(corev1.TolerationOperator("Exists")))
}

func TestGatewayReconciler_DeploymentPatchesMultiple(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-multi", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-patches-multi-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Deployment: &apiv1.DeploymentConfig{
				Patches: []apiv1.JSONPatchOperation{
					{
						Op:    "add",
						Path:  "/spec/replicas",
						Value: &apiextensionsv1.JSON{Raw: []byte(`3`)},
					},
					{
						Op:    "add",
						Path:  "/spec/template/spec/tolerations",
						Value: &apiextensionsv1.JSON{Raw: []byte(`[{"key":"node-role","operator":"Exists"}]`)},
					},
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-multi",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify both replicas and tolerations are applied
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(3)))
	g.Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Tolerations[0].Key).To(Equal("node-role"))
}

func TestGatewayReconciler_DeploymentPatchesResourceRequests(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-resources", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-patches-resources-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Deployment: &apiv1.DeploymentConfig{
				Patches: []apiv1.JSONPatchOperation{
					{
						Op:    "add",
						Path:  "/spec/template/spec/containers/0/resources",
						Value: &apiextensionsv1.JSON{Raw: []byte(`{"requests":{"memory":"128Mi","cpu":"250m"},"limits":{"memory":"256Mi"}}`)},
					},
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-resources",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify Deployment container has resource requests/limits
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))
	resources := deploy.Spec.Template.Spec.Containers[0].Resources
	g.Expect(resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(resources.Requests.Cpu().String()).To(Equal("250m"))
	g.Expect(resources.Limits.Memory().String()).To(Equal("256Mi"))
}

func TestGatewayReconciler_DeploymentPatchesInvalidYAML(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-invalid-yaml", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-patches-invalid-yaml-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Deployment: &apiv1.DeploymentConfig{
				Patches: []apiv1.JSONPatchOperation{
					{
						Op:    "replace",
						Path:  "/spec/nonexistent/field",
						Value: &apiextensionsv1.JSON{Raw: []byte(`"something"`)},
					},
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-invalid-yaml",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(ready.Message).To(ContainSubstring("deployment patches"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeploymentPatchesInvalidOps(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-invalid-ops", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-patches-invalid-ops-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Deployment: &apiv1.DeploymentConfig{
				Patches: []apiv1.JSONPatchOperation{
					{
						Op:    "replace",
						Path:  "/spec/nonexistent/field",
						Value: &apiextensionsv1.JSON{Raw: []byte(`"something"`)},
					},
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-invalid-ops",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(ready.Message).To(ContainSubstring("deployment patches"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeletionReconcileDisabled(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-disabled", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	secretKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
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
	_ = testClient.Delete(testCtx, &deploy)
	_ = testClient.Delete(testCtx, &tokenSecret)
}

func TestGatewayReconciler_DeploymentProgressDeadlineExceeded(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-deadline", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-deadline", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed (background goroutine sets Available=True).
	waitForGatewayProgrammed(g, gw)

	// Now simulate a progress deadline exceeded by patching the Deployment's
	// Progressing condition to False. The background goroutine skips Deployments
	// where ReadyReplicas == desired, so after Programmed we can safely
	// patch only the Progressing condition.
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
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

func TestGatewayReconciler_GatewayClassNoParametersRef(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

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
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-infra-invalid", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)

	// Create two GatewayClasses.
	gcA := createTestGatewayClass(g, "test-gw-class-switch-a", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gcA), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	gcB := createTestGatewayClass(g, "test-gw-class-switch-b", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gcB), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gcA)
	waitForGatewayClassReady(g, gcB)

	// Gateway referencing gc-A.
	gw := createTestGateway(g, "test-gw-switch", ns.Name, gcA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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

func TestGatewayReconciler_ReconcileDisabled(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-disabled", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-disabled", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			// Remove disabled annotation so cleanup can proceed.
			delete(latest.Annotations, apiv1.AnnotationReconcile)
			_ = testClient.Update(testCtx, &latest)
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-tunnel-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-token-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.getTunnelTokenErr = fmt.Errorf("token retrieval failed")

	gw := createTestGateway(g, "test-gw-token-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.getTunnelTokenErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-config-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Inject config update error before creating Gateway so the initial
	// tunnel configuration update (catch-all for sidecar or 404 fallback)
	// fails on the first reconcile.
	testMock.updateTunnelConfigErr = fmt.Errorf("config update failed")

	gw := createTestGateway(g, "test-gw-config-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.updateTunnelConfigErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(ready.Message).To(ContainSubstring("configuration: config update failed"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareDNSFindZoneError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-zone-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-dns-zone-err-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-zone-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
		testMock.findZoneIDErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-ensure-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-dns-ensure-err-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-ensure-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
		testMock.ensureDNSErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-tunnel-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-list-zones-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-list-zones-err-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-list-zones-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(ready.Message).To(ContainSubstring("cleaning up DNS for tunnel"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Clear error so cleanup (resetMockErrors) doesn't leave a stuck Gateway.
	// The controller will eventually retry finalization via backoff.
	testMock.listZoneIDsErr = nil
}

func TestGatewayReconciler_CloudflareCreateTunnelError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-create-tunnel-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-get-config-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-get-config-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.getTunnelConfigurationErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(ready.Message).To(ContainSubstring("configuration: config fetch failed"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareListDNSCNAMEsError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-list-cnames-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-list-cnames-err-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-list-cnames-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
		testMock.zoneIDs = nil
		testMock.listDNSCNAMEsByTargetErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Inject error and create HTTPRoute.
	testMock.zoneIDs = []string{"test-zone-id"}
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-cname-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Pre-populate the mock with a stale DNS record and set delete error.
	testMock.zoneIDs = []string{"test-zone-id"}
	testMock.listDNSCNAMEsByTarget = []string{"stale-del-err.example.com"}
	testMock.deleteDNSErr = fmt.Errorf("DNS delete failed")

	params := createTestParameters(g, "test-gw-del-cname-err-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-del-cname-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
		testMock.zoneIDs = nil
		testMock.listDNSCNAMEsByTarget = nil
		testMock.deleteDNSErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-factory-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.newClientErr = fmt.Errorf("client creation failed")

	gw := createTestGateway(g, "test-gw-factory-err", ns.Name, gc.Name)
	t.Cleanup(func() {
		testMock.newClientErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-zone-rm-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-zone-rm-err-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-zone-rm-err",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
		testMock.listZoneIDsErr = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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

	// Set ListZoneIDs error and disable DNS by setting empty zones.
	testMock.listZoneIDsErr = fmt.Errorf("zone listing failed")

	g.Eventually(func(g Gomega) {
		var latestParams apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(params), &latestParams)).To(Succeed())
		latestParams.Spec.DNS = &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{}}
		g.Expect(testClient.Update(testCtx, &latestParams)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning event on Gateway with DNS cleanup error (non-fatal).
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionReconcile, "list zone IDs")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareCleanupConnectionsErrorDuringFinalization(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-conn-cleanup-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(ready.Message).To(ContainSubstring("cleaning up connections for tunnel"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Warning/ProgressingWithRetry/Finalize event.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, apiv1.EventActionFinalize, "")
		g.Expect(e).NotTo(BeNil())
		g.Expect(e.Note).To(ContainSubstring("Finalization failed"))
		g.Expect(e.Note).To(ContainSubstring("cleaning up connections for tunnel"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Clear error so cleanup (resetMockErrors) doesn't leave a stuck Gateway.
	testMock.cleanupTunnelConnectionsErr = nil
}

func TestGatewayReconciler_CloudflareTunnelLookupErrorDuringFinalization(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-fin-lookup-err", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-fin-lookup-err", ns.Name, gc.Name)

	waitForGatewayProgrammed(g, gw)
	gwKey := client.ObjectKeyFromObject(gw)

	// Set deleteTunnel error (will hit during finalization when deleting
	// CGS-tracked tunnels).
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
	testMock.deleteTunnelErr = nil
}

func TestGatewayReconciler_DNSZoneChange(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-zone-change", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Set up zone ID mapping so old and new zones have distinct IDs.
	testMock.zones = map[string]string{
		"old.example.com": "old-zone-id",
		"new.example.com": "new-zone-id",
	}

	params := createTestParameters(g, "test-gw-zone-change-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "old.example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-zone-change",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
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
		testMock.zones = nil
		testMock.zoneIDs = nil
		testMock.listDNSCNAMEsByTarget = nil
		testMock.deleteDNSCalls = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Set mock to return existing CNAMEs (as if records existed in the old zone)
	// and clear deleteDNSCalls to get a clean slate for assertions.
	testMock.zoneIDs = []string{"old-zone-id", "new-zone-id"}
	testMock.listDNSCNAMEsByTarget = []string{"app.old.example.com"}
	testMock.deleteDNSCalls = nil

	// Change DNS zone to "new.example.com".
	g.Eventually(func(g Gomega) {
		var latestParams apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(params), &latestParams)).To(Succeed())
		latestParams.Spec.DNS.Zones = []apiv1.DNSZoneConfig{{Name: "new.example.com"}}
		g.Expect(testClient.Update(testCtx, &latestParams)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify DNS records were cleaned up in the old zone (zone ID = "old-zone-id").
	g.Eventually(func(g Gomega) {
		var oldZoneDeletes []mockDNSCall
		for _, c := range testMock.deleteDNSCalls {
			if c.ZoneID == "old-zone-id" {
				oldZoneDeletes = append(oldZoneDeletes, c)
			}
		}
		g.Expect(oldZoneDeletes).NotTo(BeEmpty())
		g.Expect(oldZoneDeletes[0].Hostname).To(Equal("app.old.example.com"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_MultipleListeners(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-multi-listener", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-multi-listener",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
				{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonListenersNotValid)))
		g.Expect(accepted.Message).To(ContainSubstring("exactly one listener"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_ParametersNotFound(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-params-not-found", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Reference a non-existent CloudflareGatewayParameters.
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-params-not-found",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef("nonexistent-params"),
			},
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("not found"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CredentialsFromCGPSecretRef(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	// Create a GatewayClass without credentials (no parametersRef).
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gw-class-cgp-secret",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for GatewayClass to be accepted (no parametersRef is fine).
	gcKey := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, gcKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create the credentials Secret that the CGP secretRef will point to.
	cgpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cgp-creds",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN":  []byte("cgp-api-token"),
			"CLOUDFLARE_ACCOUNT_ID": []byte("cgp-account-id"),
		},
	}
	g.Expect(testClient.Create(testCtx, cgpSecret)).To(Succeed())

	// Create CGP with secretRef.
	params := createTestParameters(g, "test-gw-cgp-secret-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		SecretRef: &apiv1.SecretRef{Name: "cgp-creds"},
	})

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-cgp-secret",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Gateway should be Programmed using credentials from CGP secretRef.
	waitForGatewayProgrammed(g, gw)

	// Verify the Cloudflare client was created with credentials from the CGP secret.
	g.Expect(testMock.lastClientConfig.APIToken).To(Equal("cgp-api-token"))
	g.Expect(testMock.lastClientConfig.AccountID).To(Equal("cgp-account-id"))
}

func TestGatewayReconciler_CredentialsFromCGPSecretRefMissingKeys(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gw-class-cgp-secret-missing",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gcKey := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, gcKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create Secret missing CLOUDFLARE_ACCOUNT_ID.
	incomplete := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cgp-creds-incomplete",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN": []byte("token-only"),
		},
	}
	g.Expect(testClient.Create(testCtx, incomplete)).To(Succeed())

	params := createTestParameters(g, "test-gw-cgp-secret-missing-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		SecretRef: &apiv1.SecretRef{Name: "cgp-creds-incomplete"},
	})

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-cgp-secret-missing",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}
