// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	acgatewayv1 "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

type gatewayParent struct {
	gw *gatewayv1.Gateway
	gc *gatewayv1.GatewayClass
}

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve parent Gateways that belong to our controller.
	parents, err := r.resolveParents(ctx, &route)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving parent Gateways: %w", err)
	}
	if len(parents) == 0 {
		return ctrl.Result{}, nil
	}

	// Handle deletion.
	if !route.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &route, parents)
	}

	// Add finalizer first if it doesn't exist.
	if !controllerutil.ContainsFinalizer(&route, apiv1.Finalizer) {
		patch := client.MergeFrom(route.DeepCopy())
		controllerutil.AddFinalizer(&route, apiv1.Finalizer)
		if err := r.Patch(ctx, &route, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: 1}, nil
	}

	// Skip reconciliation if disabled.
	if route.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		log.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling HTTPRoute")

	var parentStatuses []*acgatewayv1.RouteParentStatusApplyConfiguration
	for _, p := range parents {
		parentStatuses = append(parentStatuses, buildRouteParentStatus(&route, p.gw, metav1.ConditionTrue,
			string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted"))
	}

	if err := r.applyRouteStatus(ctx, &route, parentStatuses); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying route status: %w", err)
	}

	return ctrl.Result{RequeueAfter: apiv1.ReconcileInterval(route.Annotations)}, nil
}

// resolveParents returns the parent Gateways (and their GatewayClasses) that are
// managed by our controller and for which the HTTPRoute has a valid parentRef,
// including cross-namespace ReferenceGrant validation.
func (r *HTTPRouteReconciler) resolveParents(ctx context.Context, route *gatewayv1.HTTPRoute) ([]gatewayParent, error) {
	var parents []gatewayParent
	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && *ref.Group != gatewayv1.Group(gatewayv1.GroupName) {
			continue
		}
		if ref.Kind != nil && *ref.Kind != gatewayv1.Kind(apiv1.KindGateway) {
			continue
		}
		gwNamespace := route.Namespace
		if ref.Namespace != nil {
			gwNamespace = string(*ref.Namespace)
		}
		var gw gatewayv1.Gateway
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: gwNamespace,
			Name:      string(ref.Name),
		}, &gw); err != nil {
			continue
		}
		var gc gatewayv1.GatewayClass
		if err := r.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, &gc); err != nil {
			continue
		}
		if gc.Spec.ControllerName != gatewayv1.GatewayController(apiv1.ControllerName) {
			continue
		}
		granted, err := httpRouteReferenceGranted(ctx, r.Client, route.Namespace, &gw)
		if err != nil {
			return nil, fmt.Errorf("checking ReferenceGrant for Gateway %s/%s: %w", gw.Namespace, gw.Name, err)
		}
		if !granted {
			return nil, fmt.Errorf("cross-namespace reference to Gateway %s/%s not allowed by any ReferenceGrant", gw.Namespace, gw.Name)
		}
		parents = append(parents, gatewayParent{gw: gw.DeepCopy(), gc: gc.DeepCopy()})
	}
	return parents, nil
}

// finalize removes the finalizer from the HTTPRoute. DNS CNAME records are managed
// by the Gateway controller, which is triggered by the HTTPRoute deletion event via
// its Watch and will reconcile the desired DNS state for the zone.
func (r *HTTPRouteReconciler) finalize(ctx context.Context, route *gatewayv1.HTTPRoute, _ []gatewayParent) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(route, apiv1.Finalizer) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(route.DeepCopy())
	controllerutil.RemoveFinalizer(route, apiv1.Finalizer)
	if err := r.Patch(ctx, route, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// buildRouteParentStatus builds an SSA apply configuration for a single parent
// Gateway's status entry in an HTTPRoute.
func buildRouteParentStatus(route *gatewayv1.HTTPRoute, gw *gatewayv1.Gateway,
	acceptedStatus metav1.ConditionStatus, reason, message string) *acgatewayv1.RouteParentStatusApplyConfiguration {
	now := metav1.Now()
	return acgatewayv1.RouteParentStatus().
		WithParentRef(acgatewayv1.ParentReference().
			WithGroup(gatewayv1.Group(gatewayv1.GroupName)).
			WithKind(gatewayv1.Kind(apiv1.KindGateway)).
			WithNamespace(gatewayv1.Namespace(gw.Namespace)).
			WithName(gatewayv1.ObjectName(gw.Name)),
		).
		WithControllerName(gatewayv1.GatewayController(apiv1.ControllerName)).
		WithConditions(
			acmetav1.Condition().
				WithType(string(gatewayv1.RouteConditionAccepted)).
				WithStatus(acceptedStatus).
				WithObservedGeneration(route.Generation).
				WithLastTransitionTime(now).
				WithReason(reason).
				WithMessage(message),
			acmetav1.Condition().
				WithType(string(gatewayv1.RouteConditionResolvedRefs)).
				WithStatus(metav1.ConditionTrue).
				WithObservedGeneration(route.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.RouteReasonResolvedRefs)).
				WithMessage("References resolved"),
		)
}

// applyRouteStatus applies the HTTPRoute status via SSA for all parent Gateways
// in a single API call.
func (r *HTTPRouteReconciler) applyRouteStatus(ctx context.Context, route *gatewayv1.HTTPRoute,
	parentStatuses []*acgatewayv1.RouteParentStatusApplyConfiguration) error {
	statusPatch := acgatewayv1.HTTPRoute(route.Name, route.Namespace).
		WithStatus(acgatewayv1.HTTPRouteStatus().
			WithParents(parentStatuses...),
		)
	return r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership)
}

// parentRefMatches reports whether the given parentRef points to the specified Gateway,
// defaulting group to gateway.networking.k8s.io, kind to Gateway, and namespace to
// the route's namespace when unset.
func parentRefMatches(ref gatewayv1.ParentReference, gw *gatewayv1.Gateway, routeNamespace string) bool {
	if ref.Group != nil && *ref.Group != gatewayv1.Group(gatewayv1.GroupName) {
		return false
	}
	if ref.Kind != nil && *ref.Kind != gatewayv1.Kind(apiv1.KindGateway) {
		return false
	}
	if string(ref.Name) != gw.Name {
		return false
	}
	refNS := routeNamespace
	if ref.Namespace != nil {
		refNS = string(*ref.Namespace)
	}
	return refNS == gw.Namespace
}
