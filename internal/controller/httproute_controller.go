// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	acgatewayv1 "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

type gatewayParent struct {
	gw *gatewayv1.Gateway
	gc *gatewayv1.GatewayClass
}

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	events.EventRecorder
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

	// Handle deletion before any heavy work (parent resolution).
	if !route.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &route)
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

	return r.reconcile(ctx, &route)
}

func (r *HTTPRouteReconciler) reconcile(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	// Resolve parent Gateways that belong to our controller.
	parents, err := r.resolveParents(ctx, route)
	if err != nil {
		r.Eventf(route, nil, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, "Reconcile", "Reconciliation failed: %v", err)
		return ctrl.Result{}, fmt.Errorf("resolving parent Gateways: %w", err)
	}
	if len(parents) == 0 {
		return ctrl.Result{}, nil
	}

	acceptedStatus := metav1.ConditionTrue
	acceptedReason := string(gatewayv1.RouteReasonAccepted)
	acceptedMessage := "HTTPRoute is accepted"
	resolvedRefsStatus := metav1.ConditionTrue
	resolvedRefsReason := string(gatewayv1.RouteReasonResolvedRefs)
	resolvedRefsMessage := "References resolved"

	requeueAfter := apiv1.ReconcileInterval(route.Annotations)

	// Skip the status patch if no parent statuses changed.
	acceptedType := string(gatewayv1.RouteConditionAccepted)
	resolvedRefsType := string(gatewayv1.RouteConditionResolvedRefs)
	if !r.routeStatusChanged(route, parents, acceptedType, acceptedStatus, acceptedReason, acceptedMessage,
		resolvedRefsType, resolvedRefsStatus, resolvedRefsReason, resolvedRefsMessage) {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	var parentStatuses []*acgatewayv1.RouteParentStatusApplyConfiguration
	for _, p := range parents {
		parentStatuses = append(parentStatuses, buildRouteParentStatus(route, p.gw,
			acceptedStatus, acceptedReason, acceptedMessage))
	}

	statusPatch := acgatewayv1.HTTPRoute(route.Name, route.Namespace).
		WithResourceVersion(route.ResourceVersion).
		WithStatus(acgatewayv1.HTTPRouteStatus().
			WithParents(parentStatuses...),
		)
	if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ShortControllerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	r.Eventf(route, nil, corev1.EventTypeNormal, apiv1.ReasonReconciled, "Reconcile", "HTTPRoute reconciled")
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// routeStatusChanged reports whether the desired parent statuses differ from
// the existing status. It checks that the set of parents managed by our
// controller matches and that each parent's conditions are unchanged.
func (r *HTTPRouteReconciler) routeStatusChanged(route *gatewayv1.HTTPRoute, parents []gatewayParent,
	acceptedType string, acceptedStatus metav1.ConditionStatus, acceptedReason, acceptedMessage string,
	resolvedRefsType string, resolvedRefsStatus metav1.ConditionStatus, resolvedRefsReason, resolvedRefsMessage string,
) bool {
	// Count existing entries for our controller.
	var ours int
	for _, p := range route.Status.Parents {
		if p.ControllerName == apiv1.ControllerName {
			ours++
		}
	}
	if ours != len(parents) {
		return true
	}

	// Check each parent's conditions.
	for _, p := range parents {
		existing := findRouteParentStatus(route.Status.Parents, p.gw)
		if existing == nil {
			return true
		}
		if conditions.Changed(existing.Conditions, acceptedType, acceptedStatus, acceptedReason, acceptedMessage, route.Generation) ||
			conditions.Changed(existing.Conditions, resolvedRefsType, resolvedRefsStatus, resolvedRefsReason, resolvedRefsMessage, route.Generation) {
			return true
		}
	}
	return false
}

// findRouteParentStatus finds the RouteParentStatus entry for the given Gateway
// managed by our controller.
func findRouteParentStatus(statuses []gatewayv1.RouteParentStatus, gw *gatewayv1.Gateway) *gatewayv1.RouteParentStatus {
	for i, s := range statuses {
		if s.ControllerName != apiv1.ControllerName {
			continue
		}
		if string(s.ParentRef.Name) != gw.Name {
			continue
		}
		if s.ParentRef.Namespace != nil && string(*s.ParentRef.Namespace) == gw.Namespace {
			return &statuses[i]
		}
	}
	return nil
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
		if gc.Spec.ControllerName != apiv1.ControllerName {
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
		WithControllerName(apiv1.ControllerName).
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

// finalize removes the finalizer from the HTTPRoute. DNS CNAME records are managed
// by the Gateway controller, which is triggered by the HTTPRoute deletion event via
// its Watch and will reconcile the desired DNS state for the zone.
func (r *HTTPRouteReconciler) finalize(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
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
