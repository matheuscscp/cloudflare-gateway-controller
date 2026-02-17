// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"

	semver "github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	acgatewayv1 "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

// GatewayAPIVersion returns the parsed semver version of the
// sigs.k8s.io/gateway-api module dependency from the build info.
func GatewayAPIVersion() semver.Version {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		panic("failed to read build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "sigs.k8s.io/gateway-api" {
			v, err := semver.NewVersion(dep.Version)
			if err != nil {
				panic(fmt.Sprintf("failed to parse gateway-api version '%s': %v", dep.Version, err))
			}
			return *v
		}
	}
	panic("gateway-api dependency not found in build info")
}

// GatewayClassReconciler reconciles GatewayClass objects.
type GatewayClassReconciler struct {
	client.Client
	events.EventRecorder
	GatewayAPIVersion semver.Version
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var gc gatewayv1.GatewayClass
	if err := r.Get(ctx, req.NamespacedName, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if gc.Spec.ControllerName != gatewayv1.GatewayController(apiv1.ControllerName) {
		return ctrl.Result{}, nil
	}

	if !gc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Skip reconciliation if the object is suspended.
	if gc.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		log.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling GatewayClass")

	return r.reconcile(ctx, &gc)
}

func (r *GatewayClassReconciler) reconcile(ctx context.Context, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	supportedVersion, supportedVersionMessage := r.checkSupportedVersion(ctx)
	validParams, validParamsMessage := r.checkParametersRef(ctx, gc)

	acceptedStatus := metav1.ConditionTrue
	acceptedReason := string(gatewayv1.GatewayClassReasonAccepted)
	acceptedMessage := "GatewayClass is accepted"
	supportedVersionStatus := metav1.ConditionTrue
	supportedVersionReason := string(gatewayv1.GatewayClassReasonSupportedVersion)
	readyStatus := metav1.ConditionTrue
	readyReason := apiv1.ReasonReconciled
	readyMessage := "GatewayClass is ready"
	switch {
	case !supportedVersion:
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
		acceptedMessage = supportedVersionMessage
		supportedVersionStatus = metav1.ConditionFalse
		supportedVersionReason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
		readyStatus = metav1.ConditionFalse
		readyReason = apiv1.ReasonFailed
		readyMessage = supportedVersionMessage
	case !validParams:
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = string(gatewayv1.GatewayClassReasonInvalidParameters)
		acceptedMessage = validParamsMessage
		readyStatus = metav1.ConditionFalse
		readyReason = apiv1.ReasonInvalidParams
		readyMessage = validParamsMessage
	}

	requeueAfter := apiv1.ReconcileInterval(gc.Annotations)

	// Desired supported features (sorted alphabetically by name as required by the spec).
	desiredFeatures := []gatewayv1.SupportedFeature{
		{Name: gatewayv1.FeatureName(features.SupportGateway)},
		{Name: gatewayv1.FeatureName(features.SupportGatewayInfrastructurePropagation)},
		{Name: gatewayv1.FeatureName(features.SupportHTTPRoute)},
		{Name: gatewayv1.FeatureName(features.SupportReferenceGrant)},
	}

	// Skip the status patch if no conditions or features changed.
	acceptedType := string(gatewayv1.GatewayClassConditionStatusAccepted)
	supportedVersionType := string(gatewayv1.GatewayClassConditionStatusSupportedVersion)
	if !conditionChanged(gc.Status.Conditions, acceptedType, acceptedStatus, acceptedReason, acceptedMessage, gc.Generation) &&
		!conditionChanged(gc.Status.Conditions, supportedVersionType, supportedVersionStatus, supportedVersionReason, supportedVersionMessage, gc.Generation) &&
		!conditionChanged(gc.Status.Conditions, apiv1.ConditionReady, readyStatus, readyReason, readyMessage, gc.Generation) &&
		slices.Equal(gc.Status.SupportedFeatures, desiredFeatures) {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	now := metav1.Now()
	featurePatches := make([]*acgatewayv1.SupportedFeatureApplyConfiguration, len(desiredFeatures))
	for i, f := range desiredFeatures {
		featurePatches[i] = acgatewayv1.SupportedFeature().WithName(f.Name)
	}
	statusPatch := acgatewayv1.GatewayClass(gc.Name).
		WithResourceVersion(gc.ResourceVersion).
		WithStatus(acgatewayv1.GatewayClassStatus().
			WithConditions(
				acmetav1.Condition().
					WithType(acceptedType).
					WithStatus(acceptedStatus).
					WithObservedGeneration(gc.Generation).
					WithLastTransitionTime(now).
					WithReason(acceptedReason).
					WithMessage(acceptedMessage),
				acmetav1.Condition().
					WithType(supportedVersionType).
					WithStatus(supportedVersionStatus).
					WithObservedGeneration(gc.Generation).
					WithLastTransitionTime(now).
					WithReason(supportedVersionReason).
					WithMessage(supportedVersionMessage),
				acmetav1.Condition().
					WithType(apiv1.ConditionReady).
					WithStatus(readyStatus).
					WithObservedGeneration(gc.Generation).
					WithLastTransitionTime(now).
					WithReason(readyReason).
					WithMessage(readyMessage),
			).
			WithSupportedFeatures(featurePatches...),
		)

	if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	if readyStatus == metav1.ConditionFalse {
		r.Eventf(gc, nil, corev1.EventTypeWarning, readyReason, "Reconcile", readyMessage)
	} else {
		r.Eventf(gc, nil, corev1.EventTypeNormal, apiv1.ReasonReconciled, "Reconcile", "GatewayClass is ready")
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// checkSupportedVersion verifies that the installed Gateway API CRD major.minor
// version matches the version this binary was compiled against. Returns false
// with a human-readable reason if the versions are incompatible.
func (r *GatewayClassReconciler) checkSupportedVersion(ctx context.Context) (bool, string) {
	crd := &metav1.PartialObjectMetadata{}
	crd.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind(apiv1.KindCustomResourceDefinition))
	if err := r.Get(ctx, types.NamespacedName{Name: apiv1.CRDGatewayClass}, crd); err != nil {
		return false, fmt.Sprintf("Failed to get Gateway API CRD: %v", err)
	}

	bundleVersion, ok := crd.Annotations[apiv1.AnnotationBundleVersion]
	if !ok {
		return false, fmt.Sprintf("Gateway API CRD is missing %s annotation", apiv1.AnnotationBundleVersion)
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

// checkParametersRef validates the GatewayClass parametersRef. Returns false
// with a human-readable reason if the reference is invalid, nonexistent, or
// points to a malformed Secret.
func (r *GatewayClassReconciler) checkParametersRef(ctx context.Context, gc *gatewayv1.GatewayClass) (bool, string) {
	ref := gc.Spec.ParametersRef
	if ref == nil {
		return true, "No parametersRef configured"
	}

	if string(ref.Kind) != apiv1.KindSecret || (ref.Group != "" && ref.Group != "core" && ref.Group != gatewayv1.Group("")) {
		return false, fmt.Sprintf("parametersRef must reference a core/v1 Secret, got %s/%s", ref.Group, ref.Kind)
	}

	if ref.Namespace == nil {
		return false, "parametersRef must specify a namespace (Secret is a namespaced resource)"
	}

	var secret corev1.Secret
	secretKey := types.NamespacedName{Namespace: string(*ref.Namespace), Name: ref.Name}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("Secret %s/%s not found", *ref.Namespace, ref.Name)
		}
		return false, fmt.Sprintf("Failed to get Secret %s/%s: %v", *ref.Namespace, ref.Name, err)
	}

	if len(secret.Data["CLOUDFLARE_API_TOKEN"]) == 0 || len(secret.Data["CLOUDFLARE_ACCOUNT_ID"]) == 0 {
		return false, fmt.Sprintf("Secret %s/%s must contain CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID", *ref.Namespace, ref.Name)
	}

	return true, "parametersRef is valid"
}
