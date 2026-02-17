// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

// Index field constants. Named as index<Kind><Field>.
const (
	indexHTTPRouteParentGateway       = ".spec.parentRefs[gateway]"
	indexGatewayClassControllerName   = ".spec.controllerName"
	indexGatewayClassParametersRef    = ".spec.parametersRef"
	indexGatewayClassGatewayFinalizer = ".metadata.finalizers[gateway]"
)

// SetupIndexes registers all shared cache indexes.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) {
	// Index HTTPRoutes by their parent Gateway references (ns/name) and by
	// existing status.parents entries managed by our controller. Including
	// status.parents ensures that when a parentRef is removed, the old Gateway
	// is still notified so it can clean up stale status entries.
	mgr.GetCache().IndexField(ctx, &gatewayv1.HTTPRoute{}, indexHTTPRouteParentGateway,
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
		})

	// Index GatewayClasses by spec.controllerName.
	mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexGatewayClassControllerName,
		func(obj client.Object) []string {
			gc := obj.(*gatewayv1.GatewayClass)
			return []string{string(gc.Spec.ControllerName)}
		})

	// Index GatewayClasses by spec.parametersRef (Secret namespace/name).
	mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexGatewayClassParametersRef,
		func(obj client.Object) []string {
			gc := obj.(*gatewayv1.GatewayClass)
			if gc.Spec.ParametersRef == nil ||
				gc.Spec.ParametersRef.Group != "" ||
				string(gc.Spec.ParametersRef.Kind) != apiv1.KindSecret ||
				gc.Spec.ParametersRef.Namespace == nil {
				return nil
			}
			return []string{string(*gc.Spec.ParametersRef.Namespace) + "/" + string(gc.Spec.ParametersRef.Name)}
		})

	// Index GatewayClasses by per-gateway finalizers so we can efficiently
	// find and clean up stale finalizers when gatewayClassName changes.
	gatewayFinalizerPrefix := string(gatewayv1.GatewayClassFinalizerGatewaysExist) + "/"
	mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexGatewayClassGatewayFinalizer,
		func(obj client.Object) []string {
			var keys []string
			for _, f := range obj.GetFinalizers() {
				if strings.HasPrefix(f, gatewayFinalizerPrefix) {
					keys = append(keys, f)
				}
			}
			return keys
		})
}
