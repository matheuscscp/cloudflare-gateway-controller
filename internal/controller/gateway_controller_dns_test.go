// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

func TestGatewayReconciler_DNSReconciliation(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-dns-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns",
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-stale", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Pre-populate the mock with a stale DNS record
	testMock.zoneIDs = []string{"test-zone-id"}
	testMock.listDNSCNAMEsByTarget = []string{"stale.example.com"}
	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil

	params := createTestParameters(g, "test-gw-dns-stale-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-stale",
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
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-skip", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	params := createTestParameters(g, "test-gw-dns-skip-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-skip",
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
			_ = testClient.Delete(testCtx, &latest)
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
		g.Expect(dns.Message).To(ContainSubstring("Skipped hostnames (not in any configured zone):\n- app.other.com"))
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

func TestGatewayReconciler_DNSHostnameConflictAcrossGateways(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-cross-conflict", ns.Name)
	t.Cleanup(func() {
		testMock.tunnelIDFunc = nil
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-dns-cross-conflict-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	testMock.tunnelIDFunc = func(name string) string {
		switch {
		case name == apiv1.TunnelName(&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "first-gw", Namespace: ns.Name}}):
			return "first-tunnel-id"
		case name == apiv1.TunnelName(&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "second-gw", Namespace: ns.Name}}):
			return "second-tunnel-id"
		default:
			return "unexpected-tunnel-id"
		}
	}

	firstGW := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "first-gw", Namespace: ns.Name},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure:   &gatewayv1.GatewayInfrastructure{ParametersRef: parametersRef(params.Name)},
			Listeners: []gatewayv1.Listener{{
				Name:     "https",
				Protocol: gatewayv1.HTTPSProtocolType,
				Port:     443,
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, firstGW)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(firstGW), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(firstGW), &result)).To(Succeed())
		g.Expect(result.Status.Addresses).To(HaveLen(1))
		g.Expect(result.Status.Addresses[0].Value).To(Equal(cloudflare.TunnelTarget("first-tunnel-id")))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	firstRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "first-route", Namespace: ns.Name},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(firstGW.Name)}},
			},
			Hostnames: []gatewayv1.Hostname{"conflict.example.com"},
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
	g.Expect(testClient.Create(testCtx, firstRoute)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(firstRoute), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(firstRoute), &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	testMock.ensureDNSCalls = nil

	secondGW := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "second-gw", Namespace: ns.Name},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Infrastructure:   &gatewayv1.GatewayInfrastructure{ParametersRef: parametersRef(params.Name)},
			Listeners: []gatewayv1.Listener{{
				Name:     "https",
				Protocol: gatewayv1.HTTPSProtocolType,
				Port:     443,
			}},
		},
	}
	g.Expect(testClient.Create(testCtx, secondGW)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(secondGW), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(secondGW), &result)).To(Succeed())
		g.Expect(result.Status.Addresses).To(HaveLen(1))
		g.Expect(result.Status.Addresses[0].Value).To(Equal(cloudflare.TunnelTarget("second-tunnel-id")))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	secondRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "second-route", Namespace: ns.Name},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(secondGW.Name)}},
			},
			Hostnames: []gatewayv1.Hostname{"conflict.example.com"},
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
	g.Expect(testClient.Create(testCtx, secondRoute)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(secondRoute), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(secondRoute), &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("Conflicting hostname"))
		g.Expect(accepted.Message).To(ContainSubstring(apiv1.KindHTTPRoute + " " + ns.Name + "/first-route"))
		g.Expect(accepted.Message).To(ContainSubstring("Gateway " + ns.Name + "/first-gw"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	g.Consistently(func() bool {
		for _, call := range testMock.ensureDNSCalls {
			if call.Hostname == "conflict.example.com" && call.Target == cloudflare.TunnelTarget("second-tunnel-id") {
				return true
			}
		}
		return false
	}).WithTimeout(2 * time.Second).WithPolling(100 * time.Millisecond).Should(BeFalse())
}

func TestGatewayReconciler_DNSZoneRemovalCleanup(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-removal", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-dns-removal-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-removal",
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
			_ = testClient.Delete(testCtx, &latest)
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

	// Disable DNS by setting an empty zones list.
	g.Eventually(func(g Gomega) {
		var latestParams apiv1.CloudflareGatewayParameters
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(params), &latestParams)).To(Succeed())
		latestParams.Spec.DNS = &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{}}
		g.Expect(testClient.Update(testCtx, &latestParams)).To(Succeed())
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

func TestGatewayReconciler_DNSMultipleHostnamesMixed(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-mixed", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	params := createTestParameters(g, "test-gw-dns-mixed-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-mixed",
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
			_ = testClient.Delete(testCtx, &latest)
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
	dnsHostnames := make([]string, 0, len(testMock.ensureDNSCalls))
	for _, call := range testMock.ensureDNSCalls {
		dnsHostnames = append(dnsHostnames, call.Hostname)
	}
	g.Expect(dnsHostnames).To(ContainElement("app.example.com"))
	g.Expect(dnsHostnames).To(ContainElement("api.example.com"))
	g.Expect(dnsHostnames).NotTo(ContainElement("app.other.com"))
}

func TestGatewayReconciler_DNSMultiZoneWithParentChild(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-mz", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Configure the mock to return distinct zone IDs per zone name.
	testMock.zones = map[string]string{
		"example.com":     "zone-parent",
		"sub.example.com": "zone-child",
		"other.com":       "zone-other",
	}
	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	// Three zones: a parent (example.com), its child (sub.example.com), and an
	// unrelated zone (other.com). hostnameInZone is single-level subdomain only,
	// so "app.sub.example.com" matches sub.example.com but NOT example.com.
	params := createTestParameters(g, "test-gw-dns-mz-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{
			{Name: "example.com"},
			{Name: "sub.example.com"},
			{Name: "other.com"},
		}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-mz",
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
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	testMock.ensureDNSCalls = nil

	// Hostnames that exercise parent/child zone matching:
	// - app.example.com       → matches example.com (single-level subdomain)
	// - app.sub.example.com   → matches sub.example.com (single-level), NOT example.com (two levels)
	// - svc.other.com         → matches other.com
	// - deep.app.example.com  → matches NOTHING (two levels below example.com, one level below sub would be wrong suffix)
	// - unrelated.net         → matches NOTHING
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-mz",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{
				"app.example.com",
				"app.sub.example.com",
				"svc.other.com",
				"deep.app.example.com",
				"unrelated.net",
			},
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

	// Wait for at least 3 DNS ensure calls (one per matching hostname).
	g.Eventually(func() int {
		return len(testMock.ensureDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 3))

	// Build a map of hostname -> zoneID from ensure calls for easy assertions.
	ensuredByHostname := make(map[string]string)
	for _, call := range testMock.ensureDNSCalls {
		ensuredByHostname[call.Hostname] = call.ZoneID
	}

	// app.example.com → zone-parent (single-level sub of example.com)
	g.Expect(ensuredByHostname).To(HaveKeyWithValue("app.example.com", "zone-parent"))
	// app.sub.example.com → zone-child (single-level sub of sub.example.com)
	g.Expect(ensuredByHostname).To(HaveKeyWithValue("app.sub.example.com", "zone-child"))
	// svc.other.com → zone-other
	g.Expect(ensuredByHostname).To(HaveKeyWithValue("svc.other.com", "zone-other"))
	// deep.app.example.com → NOT ensured (two levels below example.com)
	g.Expect(ensuredByHostname).NotTo(HaveKey("deep.app.example.com"))
	// unrelated.net → NOT ensured
	g.Expect(ensuredByHostname).NotTo(HaveKey("unrelated.net"))

	// Verify route condition reports skipped hostnames.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dns.Message).To(ContainSubstring("app.example.com"))
		g.Expect(dns.Message).To(ContainSubstring("app.sub.example.com"))
		g.Expect(dns.Message).To(ContainSubstring("svc.other.com"))
		g.Expect(dns.Message).To(ContainSubstring("Skipped hostnames"))
		g.Expect(dns.Message).To(ContainSubstring("deep.app.example.com"))
		g.Expect(dns.Message).To(ContainSubstring("unrelated.net"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

}

func TestGatewayReconciler_DNSAllZones(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-allzones", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	// CGP with no dns field → DNS enabled for all hostnames.
	params := createTestParameters(g, "test-gw-dns-allzones-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-allzones",
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

	// Verify DNSManagement=True/Managed with "All hostnames" message.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		dnsMgmt := conditions.Find(result.Status.Conditions, apiv1.ConditionDNSManagement)
		g.Expect(dnsMgmt).NotTo(BeNil())
		g.Expect(dnsMgmt.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dnsMgmt.Reason).To(Equal(apiv1.ReasonEnabled))
		g.Expect(dnsMgmt.Message).To(Equal("All hostnames"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Reset mock tracking after Gateway is programmed.
	testMock.ensureDNSCalls = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-allzones",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com", "api.other.com"},
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

	// Verify EnsureDNSCNAME was called for both hostnames.
	g.Eventually(func() int {
		return len(testMock.ensureDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 2))
	dnsHostnames := make([]string, 0, len(testMock.ensureDNSCalls))
	for _, call := range testMock.ensureDNSCalls {
		dnsHostnames = append(dnsHostnames, call.Hostname)
	}
	g.Expect(dnsHostnames).To(ContainElement("app.example.com"))
	g.Expect(dnsHostnames).To(ContainElement("api.other.com"))

	// Verify DNSRecordsApplied condition with no "Skipped" section.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).NotTo(BeNil())
		g.Expect(dns.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(dns.Message).To(ContainSubstring("app.example.com"))
		g.Expect(dns.Message).To(ContainSubstring("api.other.com"))
		g.Expect(dns.Message).NotTo(ContainSubstring("Skipped"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DNSDisabledViaEmptyZones(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-disabled", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	// CGP with dns.zones: [] → DNS disabled.
	params := createTestParameters(g, "test-gw-dns-disabled-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-disabled",
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

	// Verify DNSManagement=False/NotConfigured.
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		dnsMgmt := conditions.Find(result.Status.Conditions, apiv1.ConditionDNSManagement)
		g.Expect(dnsMgmt).NotTo(BeNil())
		g.Expect(dnsMgmt.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(dnsMgmt.Reason).To(Equal(apiv1.ReasonDisabled))
		g.Expect(dnsMgmt.Message).To(Equal("DNS management is disabled"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Reset mock tracking.
	testMock.ensureDNSCalls = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-disabled",
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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify DNSRecordsApplied condition is absent on HTTPRoute.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		dns := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)
		g.Expect(dns).To(BeNil(), "DNSRecordsApplied condition should be absent when DNS is disabled")
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify no EnsureDNSCNAME calls.
	g.Consistently(func() int {
		return len(testMock.ensureDNSCalls)
	}).WithTimeout(2 * time.Second).WithPolling(100 * time.Millisecond).Should(Equal(0))
}

func TestGatewayReconciler_DNSAllZonesSkipsUnresolvable(t *testing.T) {
	g := NewWithT(t)
	resetMockErrors(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-dns-skip-unres", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Configure mock: only "example.com" zone is known; "unknown.net" will fail.
	testMock.zones = map[string]string{
		"app.example.com": "zone-example",
	}
	testMock.findZoneIDErr = nil
	testMock.ensureDNSCalls = nil
	testMock.deleteDNSCalls = nil
	testMock.listDNSCNAMEsByTarget = nil

	// CGP with no dns field → DNS enabled for all hostnames.
	params := createTestParameters(g, "test-gw-dns-skip-unres-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-dns-skip-unres",
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
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Reset mock tracking.
	testMock.ensureDNSCalls = nil

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-dns-skip-unres",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com", "app.unknown.net"},
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

	// Verify EnsureDNSCNAME called for the resolvable hostname.
	g.Eventually(func() int {
		return len(testMock.ensureDNSCalls)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeNumerically(">=", 1))
	g.Expect(testMock.ensureDNSCalls[0].Hostname).To(Equal("app.example.com"))

	// Verify no EnsureDNSCNAME call for the unresolvable hostname.
	g.Consistently(func() bool {
		for _, call := range testMock.ensureDNSCalls {
			if call.Hostname == "app.unknown.net" {
				return true
			}
		}
		return false
	}).WithTimeout(2 * time.Second).WithPolling(100 * time.Millisecond).Should(BeFalse())
}
