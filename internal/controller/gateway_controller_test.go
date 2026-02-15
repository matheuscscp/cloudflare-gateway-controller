// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type mockTunnelClient struct {
	createTunnelID string
	tunnelToken    string
	deleteCalled   bool
	deletedID      string
}

func (m *mockTunnelClient) CreateTunnel(_ context.Context, _ string) (string, error) {
	return m.createTunnelID, nil
}

func (m *mockTunnelClient) DeleteTunnel(_ context.Context, tunnelID string) error {
	m.deleteCalled = true
	m.deletedID = tunnelID
	return nil
}

func (m *mockTunnelClient) GetTunnelToken(_ context.Context, _ string) (string, error) {
	return m.tunnelToken, nil
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func createTestNamespace(g Gomega) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	g.Expect(testClient.Create(testCtx, ns)).To(Succeed())
	return ns
}

func createTestSecret(g Gomega, namespace string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloudflare-creds",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN":  []byte("test-api-token"),
			"CLOUDFLARE_ACCOUNT_ID": []byte("test-account-id"),
		},
	}
	g.Expect(testClient.Create(testCtx, secret)).To(Succeed())
	return secret
}

func createTestGatewayClass(g Gomega, name string, secretNamespace string) *gatewayv1.GatewayClass {
	ns := gatewayv1.Namespace(secretNamespace)
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "cloudflare-creds",
				Namespace: &ns,
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	return gc
}

func waitForGatewayClassAccepted(g Gomega, gc *gatewayv1.GatewayClass) {
	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		accepted := findCondition(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayAcceptedAndProgrammed(t *testing.T) {
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

	waitForGatewayClassAccepted(g, gc)

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
		accepted := findCondition(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		programmed := findCondition(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))

		// Listener status
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		ls := result.Status.Listeners[0]
		g.Expect(ls.Name).To(Equal(gatewayv1.SectionName("http")))
		listenerAccepted := findCondition(ls.Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(listenerAccepted).NotTo(BeNil())
		g.Expect(listenerAccepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Deployment created
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	g.Expect(testClient.Get(testCtx, deployKey, &deploy)).To(Succeed())
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("cloudflare/cloudflared:latest"))

	// Verify GatewayClass has the finalizer
	var gcResult gatewayv1.GatewayClass
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &gcResult)).To(Succeed())
	g.Expect(gcResult.Finalizers).To(ContainElement(gatewayv1.GatewayClassFinalizerGatewaysExist))
}

func TestGatewayUnsupportedProtocol(t *testing.T) {
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

	waitForGatewayClassAccepted(g, gc)

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
		programmed := findCondition(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))

		// Listener Accepted=False with UnsupportedProtocol
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		listenerAccepted := findCondition(result.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
		g.Expect(listenerAccepted).NotTo(BeNil())
		g.Expect(listenerAccepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(listenerAccepted.Reason).To(Equal(string(gatewayv1.ListenerReasonUnsupportedProtocol)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayDeletion(t *testing.T) {
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

	waitForGatewayClassAccepted(g, gc)

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
		programmed := findCondition(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
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
	var gcResult gatewayv1.GatewayClass
	g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &gcResult)).To(Succeed())
	g.Expect(gcResult.Finalizers).NotTo(ContainElement(gatewayv1.GatewayClassFinalizerGatewaysExist))
}
