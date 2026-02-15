// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"runtime/debug"

	semver "github.com/Masterminds/semver/v3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	acgatewayv1 "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

const bundleVersionAnnotation = "gateway.networking.k8s.io/bundle-version"

// GatewayAPIVersion returns the parsed semver version of the
// sigs.k8s.io/gateway-api module dependency from the build info.
func GatewayAPIVersion() *semver.Version {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	for _, dep := range info.Deps {
		if dep.Path == "sigs.k8s.io/gateway-api" {
			v, err := semver.NewVersion(dep.Version)
			if err != nil {
				return nil
			}
			return v
		}
	}
	return nil
}

// GatewayClassReconciler reconciles GatewayClass objects.
type GatewayClassReconciler struct {
	client.Client
	GatewayAPIVersion *semver.Version
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

	supportedVersion, supportedVersionMessage := r.checkSupportedVersion(ctx)

	supportedVersionStatus := metav1.ConditionTrue
	supportedVersionReason := string(gatewayv1.GatewayClassReasonSupportedVersion)
	if !supportedVersion {
		supportedVersionStatus = metav1.ConditionFalse
		supportedVersionReason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
	}

	now := metav1.Now()
	statusPatch := acgatewayv1.GatewayClass(gc.Name).
		WithResourceVersion(gc.ResourceVersion).
		WithStatus(acgatewayv1.GatewayClassStatus().
			WithConditions(
				acmetav1.Condition().
					WithType(string(gatewayv1.GatewayClassConditionStatusAccepted)).
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gc.Generation).
					WithLastTransitionTime(now).
					WithReason(string(gatewayv1.GatewayClassReasonAccepted)).
					WithMessage("GatewayClass is accepted"),
				acmetav1.Condition().
					WithType(string(gatewayv1.GatewayClassConditionStatusSupportedVersion)).
					WithStatus(supportedVersionStatus).
					WithObservedGeneration(gc.Generation).
					WithLastTransitionTime(now).
					WithReason(supportedVersionReason).
					WithMessage(supportedVersionMessage),
				acmetav1.Condition().
					WithType(apiv1.ReadyCondition).
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gc.Generation).
					WithLastTransitionTime(now).
					WithReason(apiv1.ReadyReason).
					WithMessage("GatewayClass is ready"),
			),
		)

	if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GatewayClassReconciler) checkSupportedVersion(ctx context.Context) (bool, string) {
	log := log.FromContext(ctx)

	if r.GatewayAPIVersion == nil {
		return false, "Binary Gateway API version is unknown"
	}

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

	if crdVersion.Major() != r.GatewayAPIVersion.Major() || crdVersion.Minor() != r.GatewayAPIVersion.Minor() {
		return false, fmt.Sprintf("Gateway API CRD version %q does not match binary version %q (major.minor mismatch)",
			bundleVersion, r.GatewayAPIVersion.Original())
	}

	return true, fmt.Sprintf("Gateway API CRD version %q is supported", bundleVersion)
}
