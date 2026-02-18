// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"slices"

	semver "github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

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

	if gc.Spec.ControllerName != apiv1.ControllerName {
		return ctrl.Result{}, nil
	}

	if !gc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling GatewayClass")

	return r.reconcile(ctx, &gc)
}

func (r *GatewayClassReconciler) reconcile(ctx context.Context, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	supportedVersion, supportedVersionMessage := r.checkSupportedVersion(ctx)
	validParams, validParamsMessage := r.checkParametersRef(ctx, gc)

	acceptedStatus := metav1.ConditionTrue
	acceptedReason := string(gatewayv1.GatewayClassReasonAccepted)
	acceptedMessage := "GatewayClass is accepted"
	supportedVersionStatus := metav1.ConditionTrue
	supportedVersionReason := string(gatewayv1.GatewayClassReasonSupportedVersion)
	readyStatus := metav1.ConditionTrue
	readyReason := apiv1.ReasonReconciliationSucceeded
	readyMessage := "GatewayClass is ready"
	switch {
	case !supportedVersion:
		reason := string(gatewayv1.GatewayClassReasonUnsupportedVersion)

		acceptedStatus = metav1.ConditionFalse
		acceptedReason = reason
		acceptedMessage = supportedVersionMessage
		supportedVersionStatus = metav1.ConditionFalse
		supportedVersionReason = reason
		readyStatus = metav1.ConditionFalse
		readyReason = reason
		readyMessage = supportedVersionMessage
	case !validParams:
		reason := string(gatewayv1.GatewayClassReasonInvalidParameters)

		acceptedStatus = metav1.ConditionFalse
		acceptedReason = reason
		acceptedMessage = validParamsMessage
		readyStatus = metav1.ConditionFalse
		readyReason = reason
		readyMessage = validParamsMessage
	}

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
	if !conditions.Changed(gc.Status.Conditions, acceptedType, acceptedStatus, acceptedReason, acceptedMessage, gc.Generation) &&
		!conditions.Changed(gc.Status.Conditions, supportedVersionType, supportedVersionStatus, supportedVersionReason, supportedVersionMessage, gc.Generation) &&
		!conditions.Changed(gc.Status.Conditions, apiv1.ConditionReady, readyStatus, readyReason, readyMessage, gc.Generation) &&
		slices.Equal(gc.Status.SupportedFeatures, desiredFeatures) {
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	patch := client.MergeFrom(gc.DeepCopy())
	gc.Status.Conditions = setConditions(gc.Status.Conditions, []metav1.Condition{
		{
			Type:               acceptedType,
			Status:             acceptedStatus,
			ObservedGeneration: gc.Generation,
			LastTransitionTime: now,
			Reason:             acceptedReason,
			Message:            acceptedMessage,
		},
		{
			Type:               supportedVersionType,
			Status:             supportedVersionStatus,
			ObservedGeneration: gc.Generation,
			LastTransitionTime: now,
			Reason:             supportedVersionReason,
			Message:            supportedVersionMessage,
		},
		{
			Type:               apiv1.ConditionReady,
			Status:             readyStatus,
			ObservedGeneration: gc.Generation,
			LastTransitionTime: now,
			Reason:             readyReason,
			Message:            readyMessage,
		},
	}, now)
	gc.Status.SupportedFeatures = desiredFeatures
	if err := r.Status().Patch(ctx, gc, patch); err != nil {
		return ctrl.Result{}, err
	}

	if readyStatus == metav1.ConditionFalse {
		log.Error(fmt.Errorf("%s", readyMessage), "GatewayClass failed")
		r.Eventf(gc, nil, corev1.EventTypeWarning, readyReason,
			apiv1.EventActionReconcile, readyMessage)
	} else {
		log.Info("GatewayClass reconciled")
		r.Eventf(gc, nil, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded,
			apiv1.EventActionReconcile, "GatewayClass reconciled")
	}

	return ctrl.Result{}, nil
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
