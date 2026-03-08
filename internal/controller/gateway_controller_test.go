// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cfgo "github.com/cloudflare/cloudflare-go/v6"
	"github.com/fluxcd/pkg/ssa"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
		g.Expect(ls.Name).To(Equal(gatewayv1.SectionName("https")))
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("test-tunnel-image:latest"))

	// Verify env uses secretKeyRef, not plain Value
	env := deploy.Spec.Template.Spec.Containers[0].Env
	g.Expect(env).To(HaveLen(1))
	g.Expect(env[0].Name).To(Equal("TUNNEL_TOKEN"))
	g.Expect(env[0].Value).To(BeEmpty())
	g.Expect(env[0].ValueFrom).NotTo(BeNil())
	g.Expect(env[0].ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal("gateway-" + gw.Name))
	g.Expect(env[0].ValueFrom.SecretKeyRef.Key).To(Equal("TUNNEL_TOKEN"))

	// Verify default resource limits are set on tunnel container
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

	// Inventory lists managed Kubernetes objects.
	g.Expect(cgs.Status.Inventory).To(ConsistOf(
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionApps, Kind: apiv1.KindDeployment, Name: "gateway-" + gw.Name + "-primary"},
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionCore, Kind: apiv1.KindSecret, Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionCore, Kind: apiv1.KindConfigMap, Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionCore, Kind: apiv1.KindServiceAccount, Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionRBAC, Kind: apiv1.KindRole, Name: "gateway-" + gw.Name},
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionRBAC, Kind: apiv1.KindRoleBinding, Name: "gateway-" + gw.Name},
	))

	// CGS conditions mirror Gateway conditions.
	cgsReady := conditions.Find(cgs.Status.Conditions, apiv1.ConditionReady)
	g.Expect(cgsReady).NotTo(BeNil())
	g.Expect(cgsReady.Status).To(Equal(metav1.ConditionTrue))
}

func TestGatewayReconciler_UnsupportedPort(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-port", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-port",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     8443,
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
		g.Expect(accepted.Message).To(ContainSubstring("port 8443 is not supported, must be 443"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
		g.Expect(accepted.Message).To(ContainSubstring("Only HTTPRoute and GRPCRoute kinds are supported"))
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
		g.Expect(e.Note).To(ContainSubstring("deleted tunnel Deployment"))
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
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
			Patches: []apiv1.JSONPatchOperation{
				{
					Op:    "add",
					Path:  "/spec/template/spec/tolerations",
					Value: &apiextensionsv1.JSON{Raw: []byte(`[{"key":"example.com/special-node","operator":"Exists"}]`)},
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
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
			Patches: []apiv1.JSONPatchOperation{
				{
					Op:    "add",
					Path:  "/spec/template/spec/containers/0/resources",
					Value: &apiextensionsv1.JSON{Raw: []byte(`{"requests":{"memory":"128Mi","cpu":"250m"},"limits":{"memory":"256Mi"}}`)},
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
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
			Patches: []apiv1.JSONPatchOperation{
				{
					Op:    "replace",
					Path:  "/spec/nonexistent/field",
					Value: &apiextensionsv1.JSON{Raw: []byte(`"something"`)},
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
			Patches: []apiv1.JSONPatchOperation{
				{
					Op:    "replace",
					Path:  "/spec/nonexistent/field",
					Value: &apiextensionsv1.JSON{Raw: []byte(`"something"`)},
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

func TestGatewayReconciler_DeploymentPatchesPlacementOverride(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-patches-placement", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Patch sets nodeSelector and affinity; replica config also sets zone and
	// nodeSelector. The replica placement must win.
	params := createTestParameters(g, "test-gw-patches-placement-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Patches: []apiv1.JSONPatchOperation{
				{
					Op:   "add",
					Path: "/spec/template/spec/nodeSelector",
					Value: &apiextensionsv1.JSON{
						Raw: []byte(`{"from-patch":"true"}`),
					},
				},
				{
					Op:   "add",
					Path: "/spec/template/spec/affinity",
					Value: &apiextensionsv1.JSON{
						Raw: []byte(`{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"from-patch","operator":"In","values":["yes"]}]}]}}}`),
					},
				},
			},
			Replicas: []apiv1.ReplicaConfig{
				{
					Name:         "primary",
					Zone:         "us-west-2",
					NodeSelector: map[string]string{"from-replica": "true"},
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-patches-placement",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

	// Verify the replica placement overrode the patches.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())

	// nodeSelector should be from replica, not from patch.
	g.Expect(deploy.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"from-replica": "true"}))

	// Affinity should be the zone shorthand from replica, not from patch.
	g.Expect(deploy.Spec.Template.Spec.Affinity).NotTo(BeNil())
	g.Expect(deploy.Spec.Template.Spec.Affinity.NodeAffinity).NotTo(BeNil())
	required := deploy.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	g.Expect(required).NotTo(BeNil())
	g.Expect(required.NodeSelectorTerms).To(HaveLen(1))
	g.Expect(required.NodeSelectorTerms[0].MatchExpressions).To(HaveLen(1))
	g.Expect(required.NodeSelectorTerms[0].MatchExpressions[0].Key).To(Equal("topology.kubernetes.io/zone"))
	g.Expect(required.NodeSelectorTerms[0].MatchExpressions[0].Values).To(ConsistOf("us-west-2"))

	// Explicitly delete the Gateway and wait for finalization to complete,
	// so the async DeleteTunnel mock call doesn't leak into the next test.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Expect(testClient.Delete(testCtx, gw)).To(Succeed())
	g.Eventually(func() error {
		return testClient.Get(testCtx, gwKey, &gatewayv1.Gateway{})
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Satisfy(apierrors.IsNotFound))
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
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
		latest.Annotations[apiv1.AnnotationReconcile] = apiv1.AnnotationReconcileDisabled
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
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
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

	// Gateway with infrastructure.parametersRef.kind = "Secret" (invalid, only CGP allowed).
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
					Kind:  "Secret",
					Name:  "some-secret",
				},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

	// Assert: Accepted=False/InvalidParameters, "must reference a CloudflareGatewayParameters".
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("must reference a CloudflareGatewayParameters"))
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
		latest.Annotations[apiv1.AnnotationReconcile] = apiv1.AnnotationReconcileDisabled
		g.Expect(testClient.Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait a bit for any pending reconciliation to complete.
	time.Sleep(1 * time.Second)

	// Record the current Ready condition generation.
	var snapshot gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, gwKey, &snapshot)).To(Succeed())
	prevGen := snapshot.Status.Conditions

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

	// Status conditions should remain unchanged (reconciliation was skipped).
	g.Consistently(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(latest.Status.Conditions).To(Equal(prevGen))
	}).WithTimeout(3 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
				{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443},
				{Name: "http2", Protocol: gatewayv1.HTTPSProtocolType, Port: 8080},
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
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_Replicas(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-replicas", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-replicas-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Replicas: []apiv1.ReplicaConfig{
				{Name: "zone-a", Zone: "us-east-1"},
				{Name: "zone-b"},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-replicas",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
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

	waitForGatewayProgrammed(g, gw)

	// Verify 2 Deployments exist with replica names.
	var deployA appsv1.Deployment
	deployKeyA := client.ObjectKey{Name: "gateway-" + gw.Name + "-zone-a", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKeyA, &deployA)).To(Succeed())

	var deployB appsv1.Deployment
	deployKeyB := client.ObjectKey{Name: "gateway-" + gw.Name + "-zone-b", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKeyB, &deployB)).To(Succeed())

	// Verify zone-a has zone node affinity for us-east-1.
	g.Expect(deployA.Spec.Template.Spec.Affinity).NotTo(BeNil())
	g.Expect(deployA.Spec.Template.Spec.Affinity.NodeAffinity).NotTo(BeNil())
	required := deployA.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	g.Expect(required).NotTo(BeNil())
	g.Expect(required.NodeSelectorTerms).To(HaveLen(1))
	g.Expect(required.NodeSelectorTerms[0].MatchExpressions).To(HaveLen(1))
	g.Expect(required.NodeSelectorTerms[0].MatchExpressions[0].Key).To(Equal("topology.kubernetes.io/zone"))
	g.Expect(required.NodeSelectorTerms[0].MatchExpressions[0].Values).To(ConsistOf("us-east-1"))

	// Verify zone-b has no affinity.
	g.Expect(deployB.Spec.Template.Spec.Affinity).To(BeNil())

	// Verify component labels on both Deployments.
	g.Expect(deployA.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "zone-a"))
	g.Expect(deployB.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "zone-b"))

	// Verify single shared Secret.
	var tokenSecret corev1.Secret
	secretKey := client.ObjectKey{Name: "gateway-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, secretKey, &tokenSecret)).To(Succeed())
	g.Expect(tokenSecret.Data).To(HaveKey("TUNNEL_TOKEN"))

	// Verify CGS inventory lists both Deployments.
	var cgs apiv1.CloudflareGatewayStatus
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &cgs)).To(Succeed())
	g.Expect(cgs.Status.Inventory).To(ContainElement(
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionApps, Kind: apiv1.KindDeployment, Name: "gateway-" + gw.Name + "-zone-a"},
	))
	g.Expect(cgs.Status.Inventory).To(ContainElement(
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionApps, Kind: apiv1.KindDeployment, Name: "gateway-" + gw.Name + "-zone-b"},
	))
	g.Expect(cgs.Status.Inventory).To(ContainElement(
		apiv1.ResourceRef{APIVersion: apiv1.APIVersionCore, Kind: apiv1.KindSecret, Name: "gateway-" + gw.Name},
	))
}

func TestGatewayReconciler_ReplicasDefault(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-replicas-default", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-replicas-default", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayProgrammed(g, gw)

	// Verify Deployment named gateway-{gw}-primary exists.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())

	// Verify component: primary in selector labels.
	g.Expect(deploy.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "primary"))
}

func TestCGP_XValidation_DuplicateReplicaNames(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	cgp := &apiv1.CloudflareGatewayParameters{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dup-replicas",
			Namespace: ns.Name,
		},
		Spec: apiv1.CloudflareGatewayParametersSpec{
			Tunnel: &apiv1.TunnelConfig{
				Replicas: []apiv1.ReplicaConfig{
					{Name: "alpha"},
					{Name: "alpha"},
				},
			},
		},
	}
	err := testClient.Create(testCtx, cgp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid error, got: %v", err)
	g.Expect(err.Error()).To(ContainSubstring("replica names must be unique"))
}

func TestCGP_XValidation_ZoneAndAffinityMutuallyExclusive(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	cgp := &apiv1.CloudflareGatewayParameters{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-zone-affinity",
			Namespace: ns.Name,
		},
		Spec: apiv1.CloudflareGatewayParametersSpec{
			Tunnel: &apiv1.TunnelConfig{
				Replicas: []apiv1.ReplicaConfig{
					{
						Name: "bad",
						Zone: "us-east-1",
						Affinity: &corev1.Affinity{
							NodeAffinity: &corev1.NodeAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
									NodeSelectorTerms: []corev1.NodeSelectorTerm{{
										MatchExpressions: []corev1.NodeSelectorRequirement{{
											Key:      "topology.kubernetes.io/zone",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"us-east-1"},
										}},
									}},
								},
							},
						},
					},
				},
			},
		},
	}
	err := testClient.Create(testCtx, cgp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid error, got: %v", err)
	g.Expect(err.Error()).To(ContainSubstring("zone and affinity are mutually exclusive"))
}

func TestCGP_NilVsEmptySlices(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	// Test zones: nil vs empty.
	t.Run("zones nil", func(t *testing.T) {
		g := NewWithT(t)
		cgp := createTestParameters(g, "test-zones-nil", ns.Name, apiv1.CloudflareGatewayParametersSpec{})
		t.Cleanup(func() { _ = testClient.Delete(testCtx, cgp) })

		var result apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(cgp), &result)).To(Succeed())
		g.Expect(result.Spec.DNS).To(BeNil(), "absent dns should be nil")
	})

	t.Run("zones empty", func(t *testing.T) {
		g := NewWithT(t)
		cgp := createTestParameters(g, "test-zones-empty", ns.Name, apiv1.CloudflareGatewayParametersSpec{
			DNS: &apiv1.DNSConfig{
				Zones: []apiv1.DNSZoneConfig{},
			},
		})
		t.Cleanup(func() { _ = testClient.Delete(testCtx, cgp) })

		var result apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(cgp), &result)).To(Succeed())
		g.Expect(result.Spec.DNS).NotTo(BeNil(), "dns should not be nil")
		g.Expect(result.Spec.DNS.Zones).NotTo(BeNil(), "zones should be empty slice, not nil")
		g.Expect(result.Spec.DNS.Zones).To(BeEmpty())
	})

	// Test replicas: nil vs empty.
	t.Run("replicas nil", func(t *testing.T) {
		g := NewWithT(t)
		cgp := createTestParameters(g, "test-replicas-nil", ns.Name, apiv1.CloudflareGatewayParametersSpec{})
		t.Cleanup(func() { _ = testClient.Delete(testCtx, cgp) })

		var result apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(cgp), &result)).To(Succeed())
		g.Expect(result.Spec.Tunnel).To(BeNil(), "absent tunnel should be nil")
	})

	t.Run("replicas empty", func(t *testing.T) {
		g := NewWithT(t)
		cgp := createTestParameters(g, "test-replicas-empty", ns.Name, apiv1.CloudflareGatewayParametersSpec{
			Tunnel: &apiv1.TunnelConfig{
				Replicas: []apiv1.ReplicaConfig{},
			},
		})
		t.Cleanup(func() { _ = testClient.Delete(testCtx, cgp) })

		var result apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(cgp), &result)).To(Succeed())
		g.Expect(result.Spec.Tunnel).NotTo(BeNil(), "tunnel should not be nil")
		g.Expect(result.Spec.Tunnel.Replicas).NotTo(BeNil(), "replicas should be empty slice, not nil")
		g.Expect(result.Spec.Tunnel.Replicas).To(BeEmpty())
	})
}

func TestGatewayReconciler_CustomContainerResources(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-custom-resources", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-custom-resources-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-custom-resources",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

	// Verify Deployment uses custom resources instead of defaults.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))

	// tunnel container
	resources := deploy.Spec.Template.Spec.Containers[0].Resources
	g.Expect(resources.Requests.Cpu().String()).To(Equal("100m"))
	g.Expect(resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(resources.Limits.Cpu().String()).To(Equal("1"))
	g.Expect(resources.Limits.Memory().String()).To(Equal("512Mi"))
}

func TestGatewayReconciler_MinReadySeconds(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-min-ready", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	minReady := int32(30)
	params := createTestParameters(g, "test-gw-min-ready-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			MinReadySeconds: &minReady,
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-min-ready",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

	// Verify Deployment uses the configured minReadySeconds and derived progressDeadlineSeconds.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.MinReadySeconds).To(Equal(int32(30)))
	g.Expect(*deploy.Spec.ProgressDeadlineSeconds).To(Equal(int32(90))) // minReadySeconds + 60
}

func TestGatewayReconciler_DuplicateZonesRejected(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dup-zones", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-dup-zones-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{
			Zones: []apiv1.DNSZoneConfig{
				{Name: "example.com"},
				{Name: "example.com"},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dup-zones",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

	// Gateway should be rejected with Accepted=False/InvalidParameters.
	key := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("duplicate zone name"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CloudflareClientError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cf-error", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Inject Cloudflare client creation error.
	testMock.newClientErr = fmt.Errorf("simulated cloudflare client error")

	gw := createTestGateway(g, "test-gw-cf-error", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Gateway should get Ready=Unknown/ProgressingWithRetry.
	key := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("simulated cloudflare client error"))

		// Warning event should be emitted.
		e := findEvent(g, ns.Name, gw.Name, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, "", "")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Clear the error — Gateway should recover.
	testMock.newClientErr = nil
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_TunnelCreateConflictRecovery(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gc-conflict", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayClassReady(g, gc)

	// Simulate a tunnel name conflict: GetTunnelIDByName returns "" (no tunnel),
	// CreateTunnel returns 409 Conflict, then the retry GetTunnelIDByName succeeds.
	testMock.tunnelIDFunc = func(_ string) string { return "conflict-tunnel-id" }
	testMock.createTunnelErr = &cfgo.Error{StatusCode: http.StatusConflict}

	gw := createTestGateway(g, "test-gw-conflict", ns.Name, gc.Name)
	t.Cleanup(func() {
		// Clear mock errors before deletion so finalization can run.
		testMock.createTunnelErr = nil
		testMock.tunnelIDFunc = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Gateway should reach Programmed with the conflict-resolved tunnel ID.
	key := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(result.Status.Addresses).To(HaveLen(1))
		g.Expect(result.Status.Addresses[0].Value).To(Equal(cloudflare.TunnelTarget("conflict-tunnel-id")))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_CGPWithoutSecretRefFallsBackToGatewayClass(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	// Create a GatewayClass WITHOUT parametersRef.
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gc-cgp-fallback",
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
	waitForGatewayClassReady(g, gc)

	// Create a CGP without secretRef — credentials fall through to GatewayClass,
	// which also has no parametersRef, so it should fail.
	params := createTestParameters(g, "test-params-cgp-fallback", ns.Name, apiv1.CloudflareGatewayParametersSpec{})

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-cgp-fallback",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{{
				Name:     "https",
				Protocol: gatewayv1.HTTPSProtocolType,
				Port:     443,
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Gateway should fail: CGP has no secretRef, GatewayClass has no parametersRef.
	key := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayReasonInvalidParameters)))
		g.Expect(accepted.Message).To(ContainSubstring("no parametersRef"))
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_ReplicaRemovalCleansUpStaleDeployment(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gc-replica-rm", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayClassReady(g, gc)

	// Start with 2 replicas.
	params := createTestParameters(g, "test-params-replica-rm", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Replicas: []apiv1.ReplicaConfig{
				{Name: "alpha"},
				{Name: "beta"},
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-replica-rm",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{{
				Name:     "https",
				Protocol: gatewayv1.HTTPSProtocolType,
				Port:     443,
			}},
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

	// Verify both Deployments exist.
	betaKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-beta", Namespace: ns.Name}
	g.Expect(testClient.Get(testCtx, betaKey, &appsv1.Deployment{})).To(Succeed())

	// Remove "beta" replica, keeping only "alpha".
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(params), params)).To(Succeed())
	params.Spec.Tunnel.Replicas = []apiv1.ReplicaConfig{{Name: "alpha"}}
	g.Expect(testClient.Update(testCtx, params)).To(Succeed())

	// Wait for the stale "beta" Deployment to be deleted.
	g.Eventually(func(g Gomega) {
		err := testClient.Get(testCtx, betaKey, &appsv1.Deployment{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "stale beta Deployment should be deleted")
	}).WithTimeout(15 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Gateway should still be programmed with just "alpha".
	alphaKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-alpha", Namespace: ns.Name}
	g.Expect(testClient.Get(testCtx, alphaKey, &appsv1.Deployment{})).To(Succeed())
}

func TestGatewayReconciler_TransientError(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-transient", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-transient", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Build a standalone reconciler with a fault-injecting client that fails
	// List calls, causing ensureGatewayClassFinalizer to fail and triggering
	// Gateway reconcileError (status patch succeeds).
	fc := &faultClient{
		Client: testClient,
		listInterceptor: func(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
			return fmt.Errorf("simulated transient list error")
		},
	}
	r := &controller.GatewayReconciler{
		Client:        fc,
		EventRecorder: noopEventRecorder{},
		ResourceManager: ssa.NewResourceManager(fc, nil, ssa.Owner{
			Field: apiv1.ShortControllerName,
		}),
		NewCloudflareClient: func(_ cloudflare.ClientConfig) (cloudflare.Client, error) {
			return nil, fmt.Errorf("should not be called")
		},
		TunnelImage: "test-tunnel-image:latest",
	}

	result, err := r.Reconcile(testCtx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	g.Expect(err).To(MatchError(ContainSubstring("simulated transient list error")))
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify status was best-effort patched with Ready=Unknown/ProgressingWithRetry.
	var latest gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest)).To(Succeed())
	ready := conditions.Find(latest.Status.Conditions, apiv1.ConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
	g.Expect(ready.Message).To(ContainSubstring("simulated transient list error"))
}

func TestGatewayReconciler_TransientErrorStatusPatchFails(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-transient-spf", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-transient-spf", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Fault-injecting client that fails both List (triggers reconcileError)
	// AND Status().Patch (testing the error logging path inside reconcileError
	// when the best-effort status patch itself fails).
	fc := &faultClient{
		Client: testClient,
		listInterceptor: func(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
			return fmt.Errorf("simulated list error")
		},
		statusPatchInterceptor: func(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			return fmt.Errorf("simulated status patch error")
		},
	}
	r := &controller.GatewayReconciler{
		Client:        fc,
		EventRecorder: noopEventRecorder{},
		ResourceManager: ssa.NewResourceManager(fc, nil, ssa.Owner{
			Field: apiv1.ShortControllerName,
		}),
		NewCloudflareClient: func(_ cloudflare.ClientConfig) (cloudflare.Client, error) {
			return nil, fmt.Errorf("should not be called")
		},
		TunnelImage: "test-tunnel-image:latest",
	}

	result, err := r.Reconcile(testCtx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	g.Expect(err).To(MatchError(ContainSubstring("simulated list error")))
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Status should still be Ready=True (the best-effort patch failed, so
	// the previous state from the shared reconciler is preserved).
	var latest gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest)).To(Succeed())
	ready := conditions.Find(latest.Status.Conditions, apiv1.ConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

func TestGatewayReconciler_FinalizeErrorStatusPatchFails(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-finalize-spf", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-finalize-spf", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Delete the Gateway to set DeletionTimestamp (finalizer prevents actual deletion).
	g.Expect(testClient.Delete(testCtx, gw)).To(Succeed())

	// Wait for DeletionTimestamp to be set.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest)).To(Succeed())
		g.Expect(latest.DeletionTimestamp).NotTo(BeNil())
	}).WithTimeout(5 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Use a long error message (>1024 chars) to also exercise the
	// truncateEventMessage truncation path.
	longErr := strings.Repeat("x", 1100)

	// Fault-injecting client that fails both List (causes finalizeEnabled to
	// fail) AND Status().Patch (tests the error logging path inside
	// finalizeError when the best-effort status patch itself fails).
	fc := &faultClient{
		Client: testClient,
		listInterceptor: func(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
			return fmt.Errorf("%s", longErr)
		},
		statusPatchInterceptor: func(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			return fmt.Errorf("simulated status patch error")
		},
	}
	r := &controller.GatewayReconciler{
		Client:        fc,
		EventRecorder: noopEventRecorder{},
		ResourceManager: ssa.NewResourceManager(fc, nil, ssa.Owner{
			Field: apiv1.ShortControllerName,
		}),
		NewCloudflareClient: func(_ cloudflare.ClientConfig) (cloudflare.Client, error) {
			return nil, fmt.Errorf("should not be called")
		},
		TunnelImage: "test-tunnel-image:latest",
	}

	result, err := r.Reconcile(testCtx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	g.Expect(err).To(MatchError(ContainSubstring(longErr[:50])))
	g.Expect(result).To(Equal(ctrl.Result{}))
}

func TestGatewayReconciler_TokenRotation(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, ns.Name+"-gc", ns.Name)
	waitForGatewayClassReady(g, gc)
	gw := createTestGateway(g, "tok-rot", ns.Name, gc.Name)
	waitForGatewayProgrammed(g, gw)

	// The default rotation is enabled (interval=24h) even without a CGP.
	// On first reconciliation, rotation fires because no lastTokenRotatedAt exists yet.
	g.Eventually(func(g Gomega) {
		g.Expect(testMock.rotateTunnelSecretCalls).To(BeNumerically(">", 0))
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify CGS has lastTokenRotatedAt set.
	g.Eventually(func(g Gomega) {
		var cgs apiv1.CloudflareGatewayStatus
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &cgs)).To(Succeed())
		g.Expect(cgs.Status.LastTokenRotatedAt).NotTo(BeEmpty())
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Clean up.
	g.Expect(testClient.Delete(testCtx, gw)).To(Succeed())
	g.Eventually(func(g Gomega) {
		g.Expect(apierrors.IsNotFound(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), gw))).To(BeTrue())
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_TokenRotationDisabled(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, ns.Name+"-gc", ns.Name)
	waitForGatewayClassReady(g, gc)

	// Create CGP with rotation explicitly disabled.
	enabled := false
	createTestParameters(g, "tok-rot-disabled-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		Tunnel: &apiv1.TunnelConfig{
			Token: &apiv1.TokenConfig{
				Rotation: &apiv1.TokenRotationConfig{
					Enabled: &enabled,
				},
			},
		},
	})

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tok-rot-disabled",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef("tok-rot-disabled-params"),
			},
			Listeners: []gatewayv1.Listener{{
				Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443,
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	waitForGatewayProgrammed(g, gw)

	// Rotation should NOT have been called because it's disabled.
	g.Expect(testMock.rotateTunnelSecretCalls).To(Equal(0))

	// Clean up.
	g.Expect(testClient.Delete(testCtx, gw)).To(Succeed())
	g.Eventually(func(g Gomega) {
		g.Expect(apierrors.IsNotFound(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), gw))).To(BeTrue())
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_TokenRotationError(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, ns.Name+"-gc", ns.Name)
	waitForGatewayClassReady(g, gc)

	testMock.rotateTunnelSecretErr = fmt.Errorf("rotation API error")
	gw := createTestGateway(g, "tok-rot-err", ns.Name, gc.Name)

	// Should see ProgressingWithRetry due to the rotation error.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonProgressingWithRetry))
		g.Expect(ready.Message).To(ContainSubstring("rotation API error"))
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Clean up.
	testMock.rotateTunnelSecretErr = nil
	g.Expect(testClient.Delete(testCtx, gw)).To(Succeed())
	g.Eventually(func(g Gomega) {
		g.Expect(apierrors.IsNotFound(testClient.Get(testCtx, client.ObjectKeyFromObject(gw), gw))).To(BeTrue())
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_StartupProbe(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-startup-probe", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-startup-probe",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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

	// Verify startup probe is present on all Deployments.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "gateway-" + gw.Name + "-primary", Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := deploy.Spec.Template.Spec.Containers[0]
	g.Expect(container.Args).NotTo(ContainElement("--health-url"))

	g.Expect(container.StartupProbe).NotTo(BeNil())
	g.Expect(container.StartupProbe.HTTPGet).NotTo(BeNil())
	g.Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/healthz"))
	g.Expect(container.StartupProbe.PeriodSeconds).To(Equal(int32(2)))
	g.Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(60)))
}
