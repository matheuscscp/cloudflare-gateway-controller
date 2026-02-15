// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func newTestNamespace(t *testing.T) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	if err := testClient.Create(testCtx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}
	t.Cleanup(func() {
		testClient.Delete(testCtx, ns)
	})
	return ns
}

func newTestSecret(t *testing.T, namespace string) *corev1.Secret {
	t.Helper()
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
	if err := testClient.Create(testCtx, secret); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}
	t.Cleanup(func() {
		testClient.Delete(testCtx, secret)
	})
	return secret
}

func newTestGatewayClass(t *testing.T, name string, secretNamespace string) *gatewayv1.GatewayClass {
	t.Helper()
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
	if err := testClient.Create(testCtx, gc); err != nil {
		t.Fatalf("failed to create GatewayClass: %v", err)
	}
	t.Cleanup(func() {
		// Re-fetch to get latest version before deleting
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gc.Name}, &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})
	return gc
}

func waitForCondition(t *testing.T, key types.NamespacedName, conditionType string) *gatewayv1.Gateway {
	t.Helper()
	var result gatewayv1.Gateway
	for i := 0; i < 50; i++ {
		if err := testClient.Get(testCtx, key, &result); err != nil {
			t.Fatalf("failed to get Gateway: %v", err)
		}
		if meta.IsStatusConditionPresentAndEqual(result.Status.Conditions, conditionType, metav1.ConditionTrue) {
			return &result
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for condition %q on Gateway %s", conditionType, key)
	return nil
}

func TestGatewayAcceptedAndProgrammed(t *testing.T) {
	ns := newTestNamespace(t)
	newTestSecret(t, ns.Name)
	gc := newTestGatewayClass(t, "test-gw-class-happy", ns.Name)

	// Wait for GatewayClass to be accepted
	for i := 0; i < 50; i++ {
		var gcResult gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gc.Name}, &gcResult); err == nil {
			if meta.IsStatusConditionTrue(gcResult.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted)) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

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
	if err := testClient.Create(testCtx, gw); err != nil {
		t.Fatalf("failed to create Gateway: %v", err)
	}
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	result := waitForCondition(t, key, string(gatewayv1.GatewayConditionProgrammed))

	// Verify Accepted condition
	accepted := meta.FindStatusCondition(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatal("expected Accepted condition to be True")
	}

	// Verify Programmed condition
	programmed := meta.FindStatusCondition(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatal("expected Programmed condition to be True")
	}

	// Verify listener status
	if len(result.Status.Listeners) != 1 {
		t.Fatalf("expected 1 listener status, got %d", len(result.Status.Listeners))
	}
	ls := result.Status.Listeners[0]
	if ls.Name != "http" {
		t.Fatalf("expected listener name %q, got %q", "http", ls.Name)
	}
	listenerAccepted := meta.FindStatusCondition(ls.Conditions, string(gatewayv1.ListenerConditionAccepted))
	if listenerAccepted == nil || listenerAccepted.Status != metav1.ConditionTrue {
		t.Fatal("expected listener Accepted condition to be True")
	}

	// Verify Deployment created
	var deploy appsv1.Deployment
	deployKey := types.NamespacedName{Name: "cloudflared-" + gw.Name, Namespace: gw.Namespace}
	if err := testClient.Get(testCtx, deployKey, &deploy); err != nil {
		t.Fatalf("expected cloudflared Deployment to exist: %v", err)
	}
	if deploy.Spec.Template.Spec.Containers[0].Image != "cloudflare/cloudflared:latest" {
		t.Fatalf("expected cloudflared image, got %q", deploy.Spec.Template.Spec.Containers[0].Image)
	}

	// Verify GatewayClass has the finalizer
	var gcResult gatewayv1.GatewayClass
	if err := testClient.Get(testCtx, types.NamespacedName{Name: gc.Name}, &gcResult); err != nil {
		t.Fatalf("failed to get GatewayClass: %v", err)
	}
	hasFinalizer := false
	for _, f := range gcResult.Finalizers {
		if f == gatewayv1.GatewayClassFinalizerGatewaysExist {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Fatal("expected GatewayClass to have GatewaysExist finalizer")
	}
}

func TestGatewayUnsupportedProtocol(t *testing.T) {
	ns := newTestNamespace(t)
	newTestSecret(t, ns.Name)
	gc := newTestGatewayClass(t, "test-gw-class-unsupported", ns.Name)

	// Wait for GatewayClass to be accepted
	for i := 0; i < 50; i++ {
		var gcResult gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gc.Name}, &gcResult); err == nil {
			if meta.IsStatusConditionTrue(gcResult.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted)) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

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
	if err := testClient.Create(testCtx, gw); err != nil {
		t.Fatalf("failed to create Gateway: %v", err)
	}
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &latest); err == nil {
			testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed (the controller still programs the tunnel even for unsupported listeners)
	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	result := waitForCondition(t, key, string(gatewayv1.GatewayConditionProgrammed))

	// Verify listener Accepted=False with UnsupportedProtocol
	if len(result.Status.Listeners) != 1 {
		t.Fatalf("expected 1 listener status, got %d", len(result.Status.Listeners))
	}
	ls := result.Status.Listeners[0]
	listenerAccepted := meta.FindStatusCondition(ls.Conditions, string(gatewayv1.ListenerConditionAccepted))
	if listenerAccepted == nil {
		t.Fatal("expected listener Accepted condition to exist")
	}
	if listenerAccepted.Status != metav1.ConditionFalse {
		t.Fatal("expected listener Accepted condition to be False")
	}
	if listenerAccepted.Reason != string(gatewayv1.ListenerReasonUnsupportedProtocol) {
		t.Fatalf("expected reason %q, got %q", gatewayv1.ListenerReasonUnsupportedProtocol, listenerAccepted.Reason)
	}
}

func TestGatewayDeletion(t *testing.T) {
	ns := newTestNamespace(t)
	newTestSecret(t, ns.Name)
	gc := newTestGatewayClass(t, "test-gw-class-deletion", ns.Name)

	// Wait for GatewayClass to be accepted
	for i := 0; i < 50; i++ {
		var gcResult gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gc.Name}, &gcResult); err == nil {
			if meta.IsStatusConditionTrue(gcResult.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted)) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

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
	if err := testClient.Create(testCtx, gw); err != nil {
		t.Fatalf("failed to create Gateway: %v", err)
	}

	// Wait for it to be programmed
	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	waitForCondition(t, key, string(gatewayv1.GatewayConditionProgrammed))

	// Delete the Gateway
	var latest gatewayv1.Gateway
	if err := testClient.Get(testCtx, key, &latest); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	if err := testClient.Delete(testCtx, &latest); err != nil {
		t.Fatalf("failed to delete Gateway: %v", err)
	}

	// Wait for Gateway to be fully deleted
	for i := 0; i < 50; i++ {
		err := testClient.Get(testCtx, key, &latest)
		if err != nil {
			break // NotFound means it's deleted
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify mock's DeleteTunnel was called
	if !testMock.deleteCalled {
		t.Fatal("expected DeleteTunnel to be called")
	}

	// Verify GatewayClass finalizer was removed (no more gateways)
	var gcResult gatewayv1.GatewayClass
	if err := testClient.Get(testCtx, types.NamespacedName{Name: gc.Name}, &gcResult); err != nil {
		t.Fatalf("failed to get GatewayClass: %v", err)
	}
	for _, f := range gcResult.Finalizers {
		if f == gatewayv1.GatewayClassFinalizerGatewaysExist {
			t.Fatal("expected GatewayClass finalizer to be removed")
		}
	}
}
