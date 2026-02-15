// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func TestGatewayClassAccepted(t *testing.T) {
	g := NewWithT(t)

	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cloudflare",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	t.Cleanup(func() {
		testClient.Delete(testCtx, gc)
	})

	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())

		accepted := findCondition(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.GatewayClassReasonAccepted)))

		supported := findCondition(result.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		g.Expect(supported).NotTo(BeNil())
		g.Expect(supported.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(supported.Reason).To(Equal(string(gatewayv1.GatewayClassReasonSupportedVersion)))

		ready := findCondition(result.Status.Conditions, apiv1.ReadyCondition)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReadyReason))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}
