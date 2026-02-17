// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{},
			builder.WithPredicates(debugPredicate(apiv1.KindGateway, predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{})))).
		Owns(&appsv1.Deployment{},
			builder.WithPredicates(debugPredicate(apiv1.KindDeployment,
				predicate.ResourceVersionChangedPredicate{}))).
		Watches(&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(mapHTTPRouteToGateway),
			builder.WithPredicates(debugPredicate(apiv1.KindHTTPRoute,
				predicate.ResourceVersionChangedPredicate{}))).
		Complete(r)
}

// mapHTTPRouteToGateway maps an HTTPRoute event to reconcile requests for its
// parent Gateways (from spec.parentRefs) and any Gateways that have existing
// status.parents entries managed by our controller. Including status.parents
// ensures that when a parentRef is removed, the old Gateway still reconciles
// and cleans up stale status entries.
func mapHTTPRouteToGateway(_ context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	seen := make(map[types.NamespacedName]struct{})
	var requests []reconcile.Request
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
		key := types.NamespacedName{Namespace: ns, Name: string(ref.Name)}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	for _, s := range route.Status.Parents {
		if s.ControllerName != apiv1.ControllerName {
			continue
		}
		if s.ParentRef.Namespace == nil {
			continue
		}
		key := types.NamespacedName{
			Namespace: string(*s.ParentRef.Namespace),
			Name:      string(s.ParentRef.Name),
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}
