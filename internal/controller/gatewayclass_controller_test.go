// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayClassAccepted(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cloudflare",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
		},
	}
	if err := testClient.Create(testCtx, gc); err != nil {
		t.Fatalf("failed to create GatewayClass: %v", err)
	}
	t.Cleanup(func() {
		testClient.Delete(testCtx, gc)
	})

	// Wait for the controller to reconcile and set conditions.
	var result gatewayv1.GatewayClass
	key := types.NamespacedName{Name: gc.Name}
	for i := 0; i < 50; i++ {
		if err := testClient.Get(testCtx, key, &result); err != nil {
			t.Fatalf("failed to get GatewayClass: %v", err)
		}
		if meta.IsStatusConditionTrue(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	accepted := meta.FindStatusCondition(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatal("expected Accepted condition to be True")
	}
	if accepted.Reason != string(gatewayv1.GatewayClassReasonAccepted) {
		t.Fatalf("expected Accepted reason %q, got %q", gatewayv1.GatewayClassReasonAccepted, accepted.Reason)
	}

	supported := meta.FindStatusCondition(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
	if supported == nil || supported.Status != metav1.ConditionTrue {
		t.Fatal("expected SupportedVersion condition to be True")
	}
	if supported.Reason != string(gatewayv1.GatewayClassReasonSupportedVersion) {
		t.Fatalf("expected SupportedVersion reason %q, got %q", gatewayv1.GatewayClassReasonSupportedVersion, supported.Reason)
	}
}
