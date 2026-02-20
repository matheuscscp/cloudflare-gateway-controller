// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

// Index field constants. Named as index<Kind><Field>.
const (
	indexGatewayGatewayClassName            = ".gateway.gatewayClassName"
	indexGatewayTunnelTokenSecret           = ".gateway.tunnelTokenSecret"
	indexGatewayInfrastructureParametersRef = ".gateway.infrastructureParametersRef"
	indexGatewayNamespaceSelector           = ".gateway.namespaceSelector"
	indexGatewayClassControllerName         = ".gatewayClass.controllerName"
	indexGatewayClassParametersRef          = ".gatewayClass.parametersRef"
	indexGatewayClassGatewayFinalizer       = ".gatewayClass.gatewayFinalizer"
	indexHTTPRouteParentGateway             = ".httpRoute.parentGateway"
	indexHTTPRouteBackendServiceNS          = ".httpRoute.backendServiceNamespace"
)

// SetupIndexes registers all shared cache indexes.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) {
	setupGatewayIndexes(ctx, mgr)
	setupGatewayClassIndexes(ctx, mgr)
	setupHTTPRouteIndexes(ctx, mgr)
}

func setupGatewayIndexes(ctx context.Context, mgr ctrl.Manager) {
	// Index Gateways by spec.gatewayClassName so we can map GatewayClass
	// events to affected Gateways for reconciliation.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.Gateway{}, indexGatewayGatewayClassName,
		func(obj client.Object) []string {
			gw := obj.(*gatewayv1.Gateway)
			return []string{string(gw.Spec.GatewayClassName)}
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayGatewayClassName, err))
	}

	// Index Gateways by their managed tunnel token Secret name so we can
	// map Secret events to the owning Gateway for reconciliation. The
	// namespace is filtered at query time via client.InNamespace.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.Gateway{}, indexGatewayTunnelTokenSecret,
		func(obj client.Object) []string {
			gw := obj.(*gatewayv1.Gateway)
			return []string{apiv1.TunnelTokenSecretName(gw)}
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayTunnelTokenSecret, err))
	}

	// Index Gateways by their infrastructure.parametersRef name when it
	// references a CloudflareGatewayParameters, so we can map CGP events
	// to the affected Gateway for reconciliation. The namespace is filtered
	// at query time via client.InNamespace.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.Gateway{}, indexGatewayInfrastructureParametersRef,
		func(obj client.Object) []string {
			gw := obj.(*gatewayv1.Gateway)
			if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
				return nil
			}
			ref := gw.Spec.Infrastructure.ParametersRef
			if string(ref.Kind) != apiv1.KindCloudflareGatewayParameters || ref.Group != gatewayv1.Group(apiv1.Group) {
				return nil
			}
			return []string{ref.Name}
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayInfrastructureParametersRef, err))
	}

	// Index Gateways that use allowedRoutes with from=Selector on any listener,
	// so we can efficiently find them when a Namespace's labels change.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.Gateway{}, indexGatewayNamespaceSelector,
		func(obj client.Object) []string {
			gw := obj.(*gatewayv1.Gateway)
			for _, l := range gw.Spec.Listeners {
				if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil &&
					l.AllowedRoutes.Namespaces.From != nil && *l.AllowedRoutes.Namespaces.From == gatewayv1.NamespacesFromSelector {
					return []string{"true"}
				}
			}
			return nil
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayNamespaceSelector, err))
	}
}

func setupGatewayClassIndexes(ctx context.Context, mgr ctrl.Manager) {
	// Index GatewayClasses by spec.controllerName.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexGatewayClassControllerName,
		func(obj client.Object) []string {
			gc := obj.(*gatewayv1.GatewayClass)
			return []string{string(gc.Spec.ControllerName)}
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayClassControllerName, err))
	}

	// Index GatewayClasses by spec.parametersRef (Secret namespace/name).
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexGatewayClassParametersRef,
		func(obj client.Object) []string {
			gc := obj.(*gatewayv1.GatewayClass)
			if gc.Spec.ParametersRef == nil ||
				gc.Spec.ParametersRef.Group != "" ||
				string(gc.Spec.ParametersRef.Kind) != apiv1.KindSecret ||
				gc.Spec.ParametersRef.Namespace == nil {
				return nil
			}
			return []string{string(*gc.Spec.ParametersRef.Namespace) + "/" + gc.Spec.ParametersRef.Name}
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayClassParametersRef, err))
	}

	// Index GatewayClasses by per-gateway finalizers so we can efficiently
	// find and clean up stale finalizers when gatewayClassName changes.
	gatewayFinalizerPrefix := string(gatewayv1.GatewayClassFinalizerGatewaysExist) + "/"
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexGatewayClassGatewayFinalizer,
		func(obj client.Object) []string {
			var keys []string
			for _, f := range obj.GetFinalizers() {
				if strings.HasPrefix(f, gatewayFinalizerPrefix) {
					keys = append(keys, f)
				}
			}
			return keys
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexGatewayClassGatewayFinalizer, err))
	}
}

func setupHTTPRouteIndexes(ctx context.Context, mgr ctrl.Manager) {
	// Index HTTPRoutes by their parent Gateway references (ns/name) and by
	// existing status.parents entries managed by our controller. Including
	// status.parents ensures that when a parentRef is removed, the old Gateway
	// is still notified so it can clean up stale status entries.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.HTTPRoute{}, indexHTTPRouteParentGateway,
		func(obj client.Object) []string {
			route := obj.(*gatewayv1.HTTPRoute)
			seen := make(map[string]struct{})
			var keys []string
			for _, ref := range route.Spec.ParentRefs {
				if ref.Group != nil && *ref.Group != gatewayv1.Group(gatewayv1.GroupName) {
					continue
				}
				if ref.Kind != nil && *ref.Kind != gatewayv1.Kind(apiv1.KindGateway) {
					continue
				}
				ns := route.Namespace
				if ref.Namespace != nil {
					ns = string(*ref.Namespace)
				}
				key := ns + "/" + string(ref.Name)
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					keys = append(keys, key)
				}
			}
			for _, s := range route.Status.Parents {
				if s.ControllerName != apiv1.ControllerName {
					continue
				}
				if s.ParentRef.Namespace == nil {
					continue
				}
				key := string(*s.ParentRef.Namespace) + "/" + string(s.ParentRef.Name)
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					keys = append(keys, key)
				}
			}
			return keys
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexHTTPRouteParentGateway, err))
	}

	// Index HTTPRoutes by routeNamespace/backendServiceNamespace for
	// cross-namespace backend Service references. This lets us efficiently
	// map ReferenceGrant events to affected Gateways: the grant's from
	// namespace + grant's own namespace form an exact index query.
	if err := mgr.GetCache().IndexField(ctx, &gatewayv1.HTTPRoute{}, indexHTTPRouteBackendServiceNS,
		func(obj client.Object) []string {
			route := obj.(*gatewayv1.HTTPRoute)
			seen := make(map[string]struct{})
			var keys []string
			for _, rule := range route.Spec.Rules {
				for _, ref := range rule.BackendRefs {
					if ref.Namespace == nil || string(*ref.Namespace) == route.Namespace {
						continue
					}
					key := route.Namespace + "/" + string(*ref.Namespace)
					if _, ok := seen[key]; !ok {
						seen[key] = struct{}{}
						keys = append(keys, key)
					}
				}
			}
			return keys
		}); err != nil {
		panic(fmt.Sprintf("failed to setup index %s: %v", indexHTTPRouteBackendServiceNS, err))
	}
}
