// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/ssa"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestGatewayReconciler_HTTPRouteAccepted(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-httproute-accepted", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-httproute", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

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
			_ = testClient.Delete(testCtx, &latest)
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

	// Verify route ConfigMap has the correct routes with Owner.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Hostname).To(Equal("app.example.com"))
		g.Expect(cfg.Routes[0].Owner).To(Equal(ns.Name + "/test-httproute"))
		g.Expect(cfg.Routes[0].OwnerKind).To(Equal(apiv1.KindHTTPRoute))
		g.Expect(cfg.Routes[0].Backends).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Backends[0].Service).To(Equal("http://my-service." + ns.Name + ".svc.cluster.local:8080"))
		g.Expect(cfg.Routes[0].Backends[0].Weight).To(Equal(int32(1)))
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

func TestGatewayReconciler_HTTPRouteRemovesLegacyConditionOnReconcile(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-legacy-cond", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-legacy-cond-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-legacy-cond",
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

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-httproute-legacy-cond",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name: gatewayv1.ObjectName(gw.Name),
				}},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "my-service",
							Port: new(gatewayv1.PortNumber(8080)),
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

	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		g.Expect(result.Status.Parents[0].Conditions).To(HaveLen(3))
		g.Expect(conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionDNSRecordsApplied)).To(BeNil())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	g.Eventually(func(g Gomega) {
		var latest gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &latest)).To(Succeed())
		g.Expect(latest.Status.Parents).To(HaveLen(1))
		latest.Status.Parents[0].Conditions = append(latest.Status.Parents[0].Conditions, metav1.Condition{
			Type:               "LegacyCondition",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: latest.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             "Legacy",
			Message:            "legacy condition should be removed",
		})
		g.Expect(testClient.Status().Update(testCtx, &latest)).To(Succeed())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	r := &controller.GatewayReconciler{
		Client: &faultClient{
			Client: testClient,
			listHandler: func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
				switch out := list.(type) {
				case *gatewayv1.GatewayClassList:
					return testClient.List(ctx, out)
				case *gatewayv1.HTTPRouteList:
					var all gatewayv1.HTTPRouteList
					if err := testClient.List(ctx, &all); err != nil {
						return err
					}
					out.Items = nil
					for i := range all.Items {
						route := all.Items[i]
						for _, ref := range route.Spec.ParentRefs {
							if ref.Name != gatewayv1.ObjectName(gw.Name) {
								continue
							}
							if ref.Namespace != nil && string(*ref.Namespace) != gw.Namespace {
								continue
							}
							out.Items = append(out.Items, route)
							break
						}
					}
					return nil
				case *gatewayv1.GRPCRouteList:
					out.Items = nil
					return nil
				default:
					return testClient.List(ctx, list, opts...)
				}
			},
		},
		EventRecorder: noopEventRecorder{},
		ResourceManager: ssa.NewResourceManager(testClient, nil, ssa.Owner{
			Field: apiv1.ShortControllerName,
		}),
		NewCloudflareClient: func(cfg cloudflare.ClientConfig) (cloudflare.Client, error) {
			testMock.lastClientConfig = cfg
			if testMock.newClientErr != nil {
				return nil, testMock.newClientErr
			}
			return testMock, nil
		},
		TunnelImage: "test-tunnel-image:latest",
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeNumerically(">", 0))

	g.Eventually(func(g Gomega) {
		var updated gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &updated)).To(Succeed())
		g.Expect(updated.Status.Parents).To(HaveLen(1))
		g.Expect(updated.Status.Parents[0].Conditions).To(HaveLen(3))
		g.Expect(conditions.Find(updated.Status.Parents[0].Conditions, "LegacyCondition")).To(BeNil())

		accepted := conditions.Find(updated.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(updated.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceAllowAll(t *testing.T) {
	g := NewWithT(t)

	// Namespace A: where the Gateway lives (credentials are local)
	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	// Namespace B: where the HTTPRoute lives
	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-route-all", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	// Namespace B: where the HTTPRoute lives
	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-route-denied", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with default listeners (from=Same)
	gw := createTestGateway(g, "test-gw-cross-route-denied", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-cross-route-sel", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
					Name:     "https",
					Protocol: gatewayv1.HTTPSProtocolType,
					Port:     443,
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
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-httproute-delete", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-httproute-delete", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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

	// Delete the HTTPRoute
	var latest gatewayv1.HTTPRoute
	g.Expect(testClient.Get(testCtx, routeKey, &latest)).To(Succeed())
	g.Expect(testClient.Delete(testCtx, &latest)).To(Succeed())

	// Wait for HTTPRoute to be fully deleted
	g.Eventually(func() error {
		return testClient.Get(testCtx, routeKey, &latest)
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Satisfy(apierrors.IsNotFound))

	// Verify route ConfigMap has empty routes after deletion.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(BeEmpty())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeletionWithHTTPRoutes(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-del-routes", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	params := createTestParameters(g, "test-gw-del-routes-params", ns.Name, apiv1.CloudflareGatewayParametersSpec{
		DNS: &apiv1.DNSConfig{Zones: []apiv1.DNSZoneConfig{{Name: "example.com"}}},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-del-routes",
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
		g.Expect(e.Note).To(HavePrefix("Gateway finalized"))
		g.Expect(e.Note).To(ContainSubstring("deleted tunnel Deployment"))
		g.Expect(e.Note).To(ContainSubstring("deleted tunnel"))
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Verify "Removed status entry" event on the HTTPRoute.
	g.Eventually(func(g Gomega) {
		e := findEvent(g, ns.Name, route.Name, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, apiv1.EventActionReconcile, "Removed status entry")
		g.Expect(e).NotTo(BeNil())
	}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	// Cleanup.
	testMock.zoneIDs = nil
	testMock.listDNSCNAMEsByTarget = nil
	_ = testClient.Delete(testCtx, route)
}

func TestGatewayReconciler_StaleRouteParentRefRemoved(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-stale-ref", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-stale-ref", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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

	// Route ConfigMap should have empty routes.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(BeEmpty())
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceBackendDenied(t *testing.T) {
	g := NewWithT(t)

	// ns-A: where the Gateway and HTTPRoute live.
	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	// ns-B: where the Service backend lives (cross-namespace backendRef).
	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-backend-denied", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-backend-denied", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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

	// Route ConfigMap should have empty routes (denied backend excluded).
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(BeEmpty())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteCrossNamespaceBackendGranted(t *testing.T) {
	g := NewWithT(t)

	// ns-A: where the Gateway and HTTPRoute live.
	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	// ns-B: where the Service backend lives.
	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-backend-granted", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-backend-granted", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
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
			_ = testClient.Delete(testCtx, &latest)
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
	t.Cleanup(func() { _ = testClient.Delete(testCtx, refGrant) })

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

	// Route ConfigMap should contain the cross-namespace service URL.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Hostname).To(Equal("backend-granted.example.com"))
		g.Expect(cfg.Routes[0].Backends).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Backends[0].Service).To(Equal(
			"http://cross-service." + nsB.Name + ".svc.cluster.local:8080"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRoutePathMatches(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-path", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-path", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

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
								Name: "api-svc", Port: new(gatewayv1.PortNumber(80)),
							},
						}},
					},
				},
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
								Name: "web-svc", Port: new(gatewayv1.PortNumber(80)),
							},
						}},
					},
				},
			},
		},
	}
	*route.Spec.Rules[0].Matches[0].Path.Value = "/api"
	*route.Spec.Rules[1].Matches[0].Path.Value = "/web"

	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify route ConfigMap has both path prefix routes with correct backends.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(2))

		g.Expect(cfg.Routes[0].Hostname).To(Equal("path.example.com"))
		g.Expect(cfg.Routes[0].PathPrefix).To(Equal("/api"))
		g.Expect(cfg.Routes[0].Backends).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Backends[0].Service).To(Equal("http://api-svc." + ns.Name + ".svc.cluster.local:80"))

		g.Expect(cfg.Routes[1].Hostname).To(Equal("path.example.com"))
		g.Expect(cfg.Routes[1].PathPrefix).To(Equal("/web"))
		g.Expect(cfg.Routes[1].Backends).To(HaveLen(1))
		g.Expect(cfg.Routes[1].Backends[0].Service).To(Equal("http://web-svc." + ns.Name + ".svc.cluster.local:80"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteMultipleRulesAndHostnames(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-multi-rules", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-multi-rules", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// 2 rules x 2 hostnames = 4 routes.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(4))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteNoBackends(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-no-backends", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-no-backends", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

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
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Route should be rejected (Accepted=False/UnsupportedValue) because
	// zero backends is not supported.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("backendRefs: at least one backend is required"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteUnsupportedFeatures(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-route-features", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-route-features",
			Namespace: ns.Name,
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
	waitForGatewayProgrammed(g, gw)

	// Create route with unsupported features: filter, header match, method match.
	method := gatewayv1.HTTPMethodGet
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-unsupported",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"unsupported.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Filters: []gatewayv1.HTTPRouteFilter{
						{Type: gatewayv1.HTTPRouteFilterURLRewrite, URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
							Hostname: new(gatewayv1.PreciseHostname("other.example.com")),
						}},
					},
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Headers: []gatewayv1.HTTPHeaderMatch{
								{Name: "X-Test", Value: "true"},
							},
							Method: &method,
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
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify route gets Accepted=False/UnsupportedValue with all issues listed.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("filters is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("headers is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("method is not supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteMultipleBackendRefs(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-multi-backends", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-multi-backends",
			Namespace: ns.Name,
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
	waitForGatewayProgrammed(g, gw)

	weight80 := new(int32(80))
	weight20 := new(int32(20))
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-multi-backends",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"backends.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-a", Port: new(gatewayv1.PortNumber(80)),
							},
							Weight: weight80,
						}},
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-b", Port: new(gatewayv1.PortNumber(80)),
							},
							Weight: weight20,
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

	// Multiple backendRefs should be accepted.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify route ConfigMap contains both backends with correct weights.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Hostname).To(Equal("backends.example.com"))
		g.Expect(cfg.Routes[0].Backends).To(HaveLen(2))
		g.Expect(cfg.Routes[0].Backends[0].Service).To(Equal("http://svc-a." + ns.Name + ".svc.cluster.local:80"))
		g.Expect(cfg.Routes[0].Backends[0].Weight).To(Equal(int32(80)))
		g.Expect(cfg.Routes[0].Backends[1].Service).To(Equal("http://svc-b." + ns.Name + ".svc.cluster.local:80"))
		g.Expect(cfg.Routes[0].Backends[1].Weight).To(Equal(int32(20)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteNonServiceBackend(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-nonsvc", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-nonsvc",
			Namespace: ns.Name,
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
	waitForGatewayProgrammed(g, gw)

	customKind := gatewayv1.Kind("CustomBackend")
	customGroup := gatewayv1.Group("custom.example.com")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-nonsvc",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"nonsvc.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Group: &customGroup,
								Kind:  &customKind,
								Name:  "my-custom-backend",
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

	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("only core Service backends are supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteSessionPersistence(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-sp", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-sp", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	spType := gatewayv1.CookieBasedSessionPersistence
	absoluteTimeout := gatewayv1.Duration("1h")
	lifetimeType := gatewayv1.PermanentCookieLifetimeType
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-sp",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"sp.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					SessionPersistence: &gatewayv1.SessionPersistence{
						Type:            &spType,
						AbsoluteTimeout: &absoluteTimeout,
						CookieConfig: &gatewayv1.CookieConfig{
							LifetimeType: &lifetimeType,
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-a", Port: new(gatewayv1.PortNumber(80)),
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

	// sessionPersistence should be accepted.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify route ConfigMap has correct session persistence config.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].SessionPersistence).NotTo(BeNil())
		g.Expect(cfg.Routes[0].SessionPersistence.Type).To(Equal("Cookie"))
		g.Expect(cfg.Routes[0].SessionPersistence.SessionName).To(Equal("cgw-session"))
		g.Expect(cfg.Routes[0].SessionPersistence.AbsoluteTimeout).To(Equal("1h"))
		g.Expect(cfg.Routes[0].SessionPersistence.CookieLifetimeType).To(Equal("Permanent"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteSessionPersistenceIdleTimeout(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-sp-idle", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-sp-idle", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	spType := gatewayv1.CookieBasedSessionPersistence
	idleTimeout := gatewayv1.Duration("5m")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-sp-idle",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"sp-idle.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					SessionPersistence: &gatewayv1.SessionPersistence{
						Type:        &spType,
						IdleTimeout: &idleTimeout,
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-a", Port: new(gatewayv1.PortNumber(80)),
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

	// Cookie-based idleTimeout is accepted.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify route ConfigMap has correct idleTimeout.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].SessionPersistence).NotTo(BeNil())
		g.Expect(cfg.Routes[0].SessionPersistence.Type).To(Equal("Cookie"))
		g.Expect(cfg.Routes[0].SessionPersistence.IdleTimeout).To(Equal("5m"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteSessionPersistenceIdleTimeoutHeader(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-sp-idle-hdr", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-sp-idle-hdr", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	spType := gatewayv1.HeaderBasedSessionPersistence
	idleTimeout := gatewayv1.Duration("5m")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-sp-idle-hdr",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"sp-idle-hdr.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					SessionPersistence: &gatewayv1.SessionPersistence{
						Type:        &spType,
						IdleTimeout: &idleTimeout,
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc-a", Port: new(gatewayv1.PortNumber(80)),
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

	// Header-based idleTimeout is rejected.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("sessionPersistence.idleTimeout is not supported for header-based sessions"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

// TestGatewayReconciler_HTTPRouteUnsupportedFeaturesExtended covers additional
// validateHTTPRoute paths: timeouts, retry, queryParams, backendRef filters,
// non-PathPrefix path type, and parentRef port.
func TestGatewayReconciler_HTTPRouteUnsupportedFeaturesExtended(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-route-ext", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-route-ext", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create route with all remaining unsupported features.
	exactMatch := gatewayv1.PathMatchExact
	timeout := gatewayv1.Duration("5s")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route-ext",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: gatewayv1.ObjectName(gw.Name),
						Port: new(gatewayv1.PortNumber(80)),
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"ext.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Timeouts: &gatewayv1.HTTPRouteTimeouts{
						Request: &timeout,
					},
					Retry: &gatewayv1.HTTPRouteRetry{
						Codes: []gatewayv1.HTTPRouteRetryStatusCode{503},
					},
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &exactMatch,
								Value: new(string),
							},
							QueryParams: []gatewayv1.HTTPQueryParamMatch{
								{Name: "key", Value: "val"},
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-svc", Port: new(gatewayv1.PortNumber(80)),
								},
							},
							Filters: []gatewayv1.HTTPRouteFilter{
								{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Set: []gatewayv1.HTTPHeader{{Name: "X-Custom", Value: "val"}},
								}},
							},
						},
					},
				},
			},
		},
	}
	*route.Spec.Rules[0].Matches[0].Path.Value = "/exact"
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify route gets Accepted=False with all unsupported features reported.
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("parentRefs[0].port is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("timeouts is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("retry is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("queryParams is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("path.type \"Exact\" is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("backendRefs[0].filters is not supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteConflictingHostnamePath(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-conflict", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-conflict", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create first route — this one should be accepted.
	routeA := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-a",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"conflict.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc-a",
									Port: new(gatewayv1.PortNumber(80)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, routeA)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(routeA), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for route-a to be accepted.
	routeAKey := client.ObjectKeyFromObject(routeA)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeAKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Create second route with the same hostname — this one should be rejected.
	routeB := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-b",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"conflict.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc-b",
									Port: new(gatewayv1.PortNumber(80)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, routeB)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(routeB), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify route-b is rejected due to conflict.
	routeBKey := client.ObjectKeyFromObject(routeB)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeBKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("conflict.example.com"))
		g.Expect(accepted.Message).To(ContainSubstring("Conflicting hostname/path"))
		g.Expect(accepted.Message).To(ContainSubstring("claimed by"))
		g.Expect(accepted.Message).To(ContainSubstring(apiv1.KindHTTPRoute + " " + ns.Name + "/route-a"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify route-a is still accepted.
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeAKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_HTTPRouteNoMatchingSectionName(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-no-match-section", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with a listener named "https" (default from createTestGateway)
	gw := createTestGateway(g, "test-gw-no-match-section", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create an HTTPRoute with sectionName "nonexistent" (does not match "https")
	nonexistent := gatewayv1.SectionName("nonexistent")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-httproute-no-match-section",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:        gatewayv1.ObjectName(gw.Name),
						SectionName: &nonexistent,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"nomatch.example.com"},
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

	// Verify the HTTPRoute is rejected: Accepted=False/NoMatchingParent
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonNoMatchingParent)))
		g.Expect(accepted.Message).To(ContainSubstring("No listener matches the sectionName"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify the Gateway listener has 0 attached routes
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(0)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteAccepted(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-accepted", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpcroute",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: gatewayv1.ObjectName(gw.Name),
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "my-grpc-service",
									Port: new(gatewayv1.PortNumber(9090)),
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
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify GRPCRoute becomes Accepted and Ready
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(accepted.Message).To(Equal("GRPCRoute is accepted"))

		resolvedRefs := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolvedRefs).NotTo(BeNil())
		g.Expect(resolvedRefs.Status).To(Equal(metav1.ConditionTrue))

		ready := conditions.Find(result.Status.Parents[0].Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(ready.Reason).To(Equal(apiv1.ReasonReconciliationSucceeded))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify route ConfigMap has the correct gRPC route with Protocol.
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Hostname).To(Equal("grpc.example.com"))
		g.Expect(cfg.Routes[0].Protocol).To(Equal("grpc"))
		g.Expect(cfg.Routes[0].Owner).To(Equal(ns.Name + "/test-grpcroute"))
		g.Expect(cfg.Routes[0].OwnerKind).To(Equal(apiv1.KindGRPCRoute))
		g.Expect(cfg.Routes[0].Backends).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Backends[0].Service).To(Equal("http://my-grpc-service." + ns.Name + ".svc.cluster.local:9090"))
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

func TestGatewayReconciler_GRPCRouteUnsupportedFeatures(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-unsupported", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-unsupported", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// GRPCRoute with unsupported features: parentRef.port, method match, header match
	portNum := gatewayv1.PortNumber(443)
	methodType := gatewayv1.GRPCMethodMatchExact
	svcName := "mypackage.MyService"
	methodName := "MyMethod"
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-unsupported",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: gatewayv1.ObjectName(gw.Name),
						Port: &portNum,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-bad.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					Matches: []gatewayv1.GRPCRouteMatch{
						{
							Method: &gatewayv1.GRPCMethodMatch{
								Type:    &methodType,
								Service: &svcName,
								Method:  &methodName,
							},
							Headers: []gatewayv1.GRPCHeaderMatch{
								{
									Name:  "x-custom",
									Value: "v",
								},
							},
						},
					},
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc",
									Port: new(gatewayv1.PortNumber(9090)),
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
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify GRPCRoute is rejected with UnsupportedValue
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("spec.parentRefs[0].port is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("spec.rules[0].matches[0].method is not supported"))
		g.Expect(accepted.Message).To(ContainSubstring("spec.rules[0].matches[0].headers is not supported"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteDeletion(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-delete", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-delete", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-delete",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-del.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc",
									Port: new(gatewayv1.PortNumber(9090)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())

	// Wait for route to be accepted
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify route is in ConfigMap
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Protocol).To(Equal("grpc"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Delete the GRPCRoute
	g.Expect(testClient.Delete(testCtx, route)).To(Succeed())

	// Wait for it to disappear
	g.Eventually(func() bool {
		var result gatewayv1.GRPCRoute
		return apierrors.IsNotFound(testClient.Get(testCtx, routeKey, &result))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())

	// Verify route is removed from ConfigMap
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(BeEmpty())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Gateway listener has 0 attached routes
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(0)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteAndHTTPRouteSameHostname(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-http-same", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-http", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create an HTTPRoute and a GRPCRoute on the same hostname
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-http-same",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"shared.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "http-svc",
									Port: new(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, httpRoute)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.HTTPRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(httpRoute), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-same",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"shared.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "grpc-svc",
									Port: new(gatewayv1.PortNumber(9090)),
								},
							},
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, grpcRoute)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(grpcRoute), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Both routes accepted (no conflict — different protocols)
	g.Eventually(func(g Gomega) {
		var httpResult gatewayv1.HTTPRoute
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(httpRoute), &httpResult)).To(Succeed())
		g.Expect(httpResult.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(httpResult.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	g.Eventually(func(g Gomega) {
		var grpcResult gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, client.ObjectKeyFromObject(grpcRoute), &grpcResult)).To(Succeed())
		g.Expect(grpcResult.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(grpcResult.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify ConfigMap has both routes (HTTP and gRPC)
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(2))
		var httpFound, grpcFound bool
		for _, r := range cfg.Routes {
			if r.Protocol == "" && r.Hostname == "shared.example.com" {
				httpFound = true
			}
			if r.Protocol == "grpc" && r.Hostname == "shared.example.com" {
				grpcFound = true
			}
		}
		g.Expect(httpFound).To(BeTrue(), "expected HTTP route in config")
		g.Expect(grpcFound).To(BeTrue(), "expected gRPC route in config")
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Gateway listener counts both routes
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(2)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteCrossNamespaceBackendGranted(t *testing.T) {
	g := NewWithT(t)

	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-xns-grant", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-xns-grant", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// Create ReferenceGrant in nsB allowing GRPCRoute from nsA
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-grpc-backend",
			Namespace: nsB.Name,
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1beta1.Group(gatewayv1.GroupName),
					Kind:      gatewayv1beta1.Kind(apiv1.KindGRPCRoute),
					Namespace: gatewayv1beta1.Namespace(nsA.Name),
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "",
					Kind:  "Service",
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, grant)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, grant) })

	// Create GRPCRoute with cross-namespace backendRef
	nsBNS := gatewayv1.Namespace(nsB.Name)
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-xns",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-xns.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "xns-svc",
									Port:      new(gatewayv1.PortNumber(9090)),
									Namespace: &nsBNS,
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
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify ResolvedRefs=True (grant allows cross-namespace reference)
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		resolved := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolved).NotTo(BeNil())
		g.Expect(resolved.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify ConfigMap has the cross-namespace backend
	g.Eventually(func(g Gomega) {
		cfg := getRouteConfig(g, gw)
		g.Expect(cfg.Routes).To(HaveLen(1))
		g.Expect(cfg.Routes[0].Backends[0].Service).To(ContainSubstring(nsB.Name))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteCrossNamespaceBackendDenied(t *testing.T) {
	g := NewWithT(t)

	nsA := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsA) })

	nsB := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, nsB) })

	createTestSecret(g, nsA.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-xns-deny", nsA.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-xns-deny", nsA.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// No ReferenceGrant — cross-namespace reference should be denied
	nsBNS := gatewayv1.Namespace(nsB.Name)
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-xns-deny",
			Namespace: nsA.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-deny.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "xns-svc",
									Port:      new(gatewayv1.PortNumber(9090)),
									Namespace: &nsBNS,
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
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify ResolvedRefs=False/RefNotPermitted
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		resolved := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		g.Expect(resolved).NotTo(BeNil())
		g.Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(resolved.Reason).To(Equal(string(gatewayv1.RouteReasonRefNotPermitted)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_DeletionWithGRPCRoutes(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-gw-del", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-gw-del", ns.Name, gc.Name)
	waitForGatewayProgrammed(g, gw)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-gw-del",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-gwdel.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc",
									Port: new(gatewayv1.PortNumber(9090)),
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
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Wait for GRPCRoute to be accepted and have status.parents entry
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Delete the Gateway
	g.Expect(testClient.Delete(testCtx, gw)).To(Succeed())

	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func() bool {
		var result gatewayv1.Gateway
		return apierrors.IsNotFound(testClient.Get(testCtx, gwKey, &result))
	}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())

	// Verify GRPCRoute status.parents entry was removed by finalization
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(BeEmpty())
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteListenerKindRestriction(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-kind-restrict", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	// Gateway with listener that only allows HTTPRoute (not GRPCRoute)
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-grpc-kind-restrict",
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
							{Kind: apiv1.KindHTTPRoute},
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
	waitForGatewayProgrammed(g, gw)

	// Create a GRPCRoute — should be rejected since listener only allows HTTPRoute
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-kind-restrict",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-restrict.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc",
									Port: new(gatewayv1.PortNumber(9090)),
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
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	// Verify GRPCRoute is rejected: NoMatchingParent (listener doesn't allow GRPCRoute)
	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonNoMatchingParent)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	// Verify Gateway listener has 0 attached routes (GRPCRoute not counted)
	gwKey := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, gwKey, &result)).To(Succeed())
		g.Expect(result.Status.Listeners).To(HaveLen(1))
		g.Expect(result.Status.Listeners[0].AttachedRoutes).To(Equal(int32(0)))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func TestGatewayReconciler_GRPCRouteNoBackends(t *testing.T) {
	g := NewWithT(t)

	ns := createTestNamespace(g)
	t.Cleanup(func() { _ = testClient.Delete(testCtx, ns) })

	createTestSecret(g, ns.Name)
	gc := createTestGatewayClass(g, "test-gw-class-grpc-nobackend", ns.Name)
	t.Cleanup(func() {
		var latest gatewayv1.GatewayClass
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gc), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	waitForGatewayClassReady(g, gc)

	gw := createTestGateway(g, "test-gw-grpc-nobackend", ns.Name, gc.Name)
	t.Cleanup(func() {
		var latest gatewayv1.Gateway
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(gw), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})
	waitForGatewayProgrammed(g, gw)

	// GRPCRoute with zero backendRefs
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-nobackend",
			Namespace: ns.Name,
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gw.Name)},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc-nobackend.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{},
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, route)).To(Succeed())
	t.Cleanup(func() {
		var latest gatewayv1.GRPCRoute
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(route), &latest); err == nil {
			_ = testClient.Delete(testCtx, &latest)
		}
	})

	routeKey := client.ObjectKeyFromObject(route)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GRPCRoute
		g.Expect(testClient.Get(testCtx, routeKey, &result)).To(Succeed())
		g.Expect(result.Status.Parents).To(HaveLen(1))
		accepted := conditions.Find(result.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
		g.Expect(accepted).NotTo(BeNil())
		g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(accepted.Reason).To(Equal(string(gatewayv1.RouteReasonUnsupportedValue)))
		g.Expect(accepted.Message).To(ContainSubstring("at least one backend is required"))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}
