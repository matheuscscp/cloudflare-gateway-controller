// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/predicates"
)

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{},
			builder.WithPredicates(predicates.Debug(apiv1.KindGateway, predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{})))).
		Owns(&appsv1.Deployment{},
			builder.WithPredicates(predicates.Debug(apiv1.KindDeployment,
				predicate.ResourceVersionChangedPredicate{}))).
		Watches(&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(mapHTTPRouteToGateway),
			builder.WithPredicates(predicates.Debug(apiv1.KindHTTPRoute,
				predicate.ResourceVersionChangedPredicate{}))).
		WatchesMetadata(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToGateway),
			builder.WithPredicates(predicates.Debug(apiv1.KindSecret,
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

// mapSecretToGateway maps a Secret event to reconcile requests for Gateways
// whose managed tunnel token Secret matches the event object.
func (r *GatewayReconciler) mapSecretToGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{
		indexGatewayTunnelTokenSecret: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Gateways for Secret")
		return nil
	}
	var requests []reconcile.Request
	for _, gw := range gateways.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&gw),
		})
	}
	return requests
}
