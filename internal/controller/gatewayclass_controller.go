// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	semver "github.com/Masterminds/semver/v3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

const bundleVersionAnnotation = "gateway.networking.k8s.io/bundle-version"

// GatewayClassReconciler reconciles GatewayClass objects.
type GatewayClassReconciler struct {
	client.Client
	GatewayAPIVersion string
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var gc gatewayv1.GatewayClass
	if err := r.Get(ctx, req.NamespacedName, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if gc.Spec.ControllerName != gatewayv1.GatewayController(apiv1.ControllerName) {
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

	supportedVersion, supportedVersionMessage := r.checkSupportedVersion(ctx)

	supportedVersionCondition := metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		ObservedGeneration: gc.Generation,
	}
	if supportedVersion {
		supportedVersionCondition.Status = metav1.ConditionTrue
		supportedVersionCondition.Reason = string(gatewayv1.GatewayClassReasonSupportedVersion)
		supportedVersionCondition.Message = supportedVersionMessage
	} else {
		supportedVersionCondition.Status = metav1.ConditionFalse
		supportedVersionCondition.Reason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
		supportedVersionCondition.Message = supportedVersionMessage
	}
	meta.SetStatusCondition(&gc.Status.Conditions, supportedVersionCondition)

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

func (r *GatewayClassReconciler) checkSupportedVersion(ctx context.Context) (bool, string) {
	log := log.FromContext(ctx)

	var crd apiextensionsv1.CustomResourceDefinition
	if err := r.Get(ctx, types.NamespacedName{Name: "gatewayclasses.gateway.networking.k8s.io"}, &crd); err != nil {
		log.Error(err, "Failed to get Gateway API CRD")
		return false, fmt.Sprintf("Failed to get Gateway API CRD: %v", err)
	}

	bundleVersion, ok := crd.Annotations[bundleVersionAnnotation]
	if !ok {
		return false, fmt.Sprintf("Gateway API CRD is missing %s annotation", bundleVersionAnnotation)
	}

	crdVersion, err := semver.NewVersion(bundleVersion)
	if err != nil {
		return false, fmt.Sprintf("Failed to parse CRD bundle version %q: %v", bundleVersion, err)
	}

	binaryVersion, err := semver.NewVersion(r.GatewayAPIVersion)
	if err != nil {
		return false, fmt.Sprintf("Failed to parse binary Gateway API version %q: %v", r.GatewayAPIVersion, err)
	}

	if crdVersion.Major() != binaryVersion.Major() || crdVersion.Minor() != binaryVersion.Minor() {
		return false, fmt.Sprintf("Gateway API CRD version %q does not match binary version %q (major.minor mismatch)",
			bundleVersion, r.GatewayAPIVersion)
	}

	return true, fmt.Sprintf("Gateway API CRD version %q is supported", bundleVersion)
}
