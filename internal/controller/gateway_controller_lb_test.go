// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"slices"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

func TestGatewayReconciler_HighAvailability(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-ha", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Enable per-tunnel unique IDs for multi-tunnel tests.
	testMock.tunnelIDFunc = func(name string) string {
		return "tid-" + name
	}

	params := createTestParameters(g, "test-gw-ha-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zone: apiv1.DNSZoneConfig{Name: "example.com"}},
		Tunnels: &apiv1.TunnelsConfig{
			AvailabilityZones: []apiv1.AvailabilityZone{
				{Name: "az1", Zone: "us-east-1a"},
				{Name: "az2", Zone: "us-west-2a"},
			},
		},
		LoadBalancer: &apiv1.LoadBalancerConfig{
			Topology:       apiv1.LoadBalancerTopologyHighAvailability,
			SteeringPolicy: "Geographic",
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-ha",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Protocol: gatewayv1.HTTPProtocolType,
				Port:     80,
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

	// Wait for Gateway to be Programmed with 2 addresses (one per AZ tunnel).
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(result.Status.Addresses).To(HaveLen(2))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create an HTTPRoute to trigger LB reconciliation.
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-ha",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"ha.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
						},
					},
				}},
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for EnsureLoadBalancer to be called for our hostname.
	g.Eventually(func() bool {
		for _, c := range testMock.ensureLBCalls {
			if c.Hostname == "ha.example.com" {
				return true
			}
		}
		return false
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())

	// Look up the Gateway to get UID for asserting on monitor/pool names.
	var gwResult gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, gwKey, &gwResult)).To(Succeed())

	// Verify monitor was created for this Gateway.
	monitorName := apiv1.MonitorName(&gwResult)
	g.Expect(testMock.monitors).To(HaveKey(monitorName))

	// Verify pools were created (one per AZ) for this Gateway.
	g.Expect(testMock.poolsByName).To(HaveKey(apiv1.PoolNameForAZ(&gwResult, "az1")))
	g.Expect(testMock.poolsByName).To(HaveKey(apiv1.PoolNameForAZ(&gwResult, "az2")))

	// Find the EnsureLoadBalancer call for our hostname.
	var lbCall mockEnsureLBCall
	for _, c := range testMock.ensureLBCalls {
		if c.Hostname == "ha.example.com" {
			lbCall = c
			break
		}
	}
	g.Expect(lbCall.PoolIDs).To(HaveLen(2))
	g.Expect(lbCall.SteeringPolicy).To(Equal("geo"))
	g.Expect(lbCall.SessionAffinity).To(Equal("none"))
	// HighAvailability: no pool weights (nil).
	g.Expect(lbCall.PoolWeights).To(BeNil())
}

func TestGatewayReconciler_TrafficSplitting(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-ts", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Enable per-tunnel unique IDs.
	testMock.tunnelIDFunc = func(name string) string {
		return "tid-" + name
	}

	params := createTestParameters(g, "test-gw-ts-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zone: apiv1.DNSZoneConfig{Name: "example.com"}},
		LoadBalancer: &apiv1.LoadBalancerConfig{
			Topology:       apiv1.LoadBalancerTopologyTrafficSplitting,
			SteeringPolicy: "Random",
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-ts",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Protocol: gatewayv1.HTTPProtocolType,
				Port:     80,
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

	// Wait for Gateway to be Programmed. With TrafficSplitting and no routes,
	// there are no services yet so tunnels and addresses appear once routes
	// are attached. Wait for Accepted first.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		accepted := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create an HTTPRoute with 2 backend refs and weights.
	weight80 := int32(80)
	weight20 := int32(20)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-ts",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"ts.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-a", Port: new(gatewayv1.PortNumber(8080)),
							},
							Weight: &weight80,
						},
					},
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-b", Port: new(gatewayv1.PortNumber(8080)),
							},
							Weight: &weight20,
						},
					},
				},
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed with 2 addresses (one per service tunnel).
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(result.Status.Addresses).To(HaveLen(2))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait for EnsureLoadBalancer to be called for our hostname.
	g.Eventually(func() bool {
		for _, c := range testMock.ensureLBCalls {
			if c.Hostname == "ts.example.com" {
				return true
			}
		}
		return false
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())

	// Look up the Gateway to get UID for asserting on monitor/pool names.
	var gwResult gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, gwKey, &gwResult)).To(Succeed())

	// Verify monitor was created for this Gateway.
	monitorName := apiv1.MonitorName(&gwResult)
	g.Expect(testMock.monitors).To(HaveKey(monitorName))

	// Verify pools were created (one per service) for this Gateway.
	g.Expect(testMock.poolsByName).To(HaveKey(apiv1.PoolNameForService(&gwResult, "svc-a")))
	g.Expect(testMock.poolsByName).To(HaveKey(apiv1.PoolNameForService(&gwResult, "svc-b")))

	// Find the EnsureLoadBalancer call for our hostname.
	var lbCall mockEnsureLBCall
	for _, c := range testMock.ensureLBCalls {
		if c.Hostname == "ts.example.com" {
			lbCall = c
			break
		}
	}
	g.Expect(lbCall.PoolIDs).To(HaveLen(2))
	g.Expect(lbCall.SteeringPolicy).To(Equal("random"))
	g.Expect(lbCall.SessionAffinity).To(Equal("none"))

	// TrafficSplitting: pool weights should be set (80/100=0.8 and 20/100=0.2).
	g.Expect(lbCall.PoolWeights).To(HaveLen(2))
	var totalWeight float64
	for _, w := range lbCall.PoolWeights {
		totalWeight += w
	}
	g.Expect(totalWeight).To(BeNumerically("~", 1.0, 0.01))
}

func TestGatewayReconciler_LBDeletion(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-lb-del", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Enable per-tunnel unique IDs.
	testMock.tunnelIDFunc = func(name string) string {
		return "tid-" + name
	}

	params := createTestParameters(g, "test-gw-lb-del-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zone: apiv1.DNSZoneConfig{Name: "example.com"}},
		Tunnels: &apiv1.TunnelsConfig{
			AvailabilityZones: []apiv1.AvailabilityZone{
				{Name: "az1", Zone: "us-east-1a"},
			},
		},
		LoadBalancer: &apiv1.LoadBalancerConfig{
			Topology:       apiv1.LoadBalancerTopologyHighAvailability,
			SteeringPolicy: "Geographic",
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-lb-del",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Protocol: gatewayv1.HTTPProtocolType,
				Port:     80,
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())

	// Wait for Gateway to be Programmed.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Look up the Gateway to get UID for asserting on monitor/pool names.
	var gwResult gatewayv1.Gateway
	g.Expect(testClient.Get(testCtx, gwKey, &gwResult)).To(Succeed())

	// Verify monitor and pool were created for this Gateway.
	monitorName := apiv1.MonitorName(&gwResult)
	g.Expect(testMock.monitors).To(HaveKey(monitorName))
	g.Expect(testMock.poolsByName).To(HaveKey(apiv1.PoolNameForAZ(&gwResult, "az1")))

	// Set up mock to return existing LB hostnames and pools for cleanup.
	testMock.lbHostnames = []string{"del.example.com"}

	// Delete the Gateway.
	g.Eventually(func(g Gomega) {
		var latest gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &latest)).To(Succeed())
		g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Wait for the Gateway to be fully deleted (finalizer removed).
	g.Eventually(func() error {
		var latest gatewayv1.Gateway
		return testClient.Get(testCtx, gwKey, &latest)
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).ShouldNot(Succeed())

	// Verify LB resources were cleaned up during finalization.
	// Load balancer hostname should have been deleted.
	g.Expect(testMock.deleteLBCalls).To(ContainElement("del.example.com"))
	// Pool should have been deleted.
	g.Expect(testMock.deletePoolIDs).NotTo(BeEmpty())
	// Monitor should have been deleted.
	g.Expect(testMock.deleteMonitorIDs).NotTo(BeEmpty())
}

func TestGatewayReconciler_LBStaleLBCleanup(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-lb-stale", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Enable per-tunnel unique IDs.
	testMock.tunnelIDFunc = func(name string) string {
		return "tid-" + name
	}

	params := createTestParameters(g, "test-gw-lb-stale-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zone: apiv1.DNSZoneConfig{Name: "example.com"}},
		Tunnels: &apiv1.TunnelsConfig{
			AvailabilityZones: []apiv1.AvailabilityZone{
				{Name: "az1", Zone: "us-east-1a"},
			},
		},
		LoadBalancer: &apiv1.LoadBalancerConfig{
			Topology:       apiv1.LoadBalancerTopologyHighAvailability,
			SteeringPolicy: "Geographic",
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-lb-stale",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: parametersRef(params.Name),
			},
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Protocol: gatewayv1.HTTPProtocolType,
				Port:     80,
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	t.Cleanup(func() {
		testMock.lbHostnames = nil
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for Gateway to be Programmed.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Simulate a stale LB hostname (no route references it).
	testMock.lbHostnames = []string{"stale.example.com"}
	testMock.deleteLBCalls = nil

	// Create a route for a DIFFERENT hostname to trigger re-reconcile.
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-lb-stale",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"active.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "my-service", Port: new(gatewayv1.PortNumber(8080)),
						},
					},
				}},
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify the stale LB hostname is deleted.
	g.Eventually(func() bool {
		return slices.Contains(testMock.deleteLBCalls, "stale.example.com")
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
}
