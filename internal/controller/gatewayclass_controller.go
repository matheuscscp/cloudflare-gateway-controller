// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

const ControllerName = "github.com/matheuscscp/cloudflare-gateway-controller"

// GatewayClassReconciler reconciles GatewayClass objects.
type GatewayClassReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var gc gatewayv1.GatewayClass
	if err := r.Get(ctx, req.NamespacedName, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if gc.Spec.ControllerName != gatewayv1.GatewayController(ControllerName) {
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling GatewayClass")

	meta.SetStatusCondition(&gc.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gc.Generation,
		Reason:             string(gatewayv1.GatewayClassReasonAccepted),
		Message:            "GatewayClass is accepted",
	})

	meta.SetStatusCondition(&gc.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gc.Generation,
		Reason:             string(gatewayv1.GatewayClassReasonSupportedVersion),
		Message:            "Gateway API CRD version is supported",
	})

	meta.SetStatusCondition(&gc.Status.Conditions, metav1.Condition{
		Type:               apiv1.ReadyCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gc.Generation,
		Reason:             apiv1.ReadyReason,
		Message:            "GatewayClass is ready",
	})

	if err := r.Status().Update(ctx, &gc); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
