// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	acgatewayv1 "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	cfclient "github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// DefaultCloudflaredImage is the default cloudflared container image.
// This value is updated automatically by the upgrade-cloudflared workflow.
const DefaultCloudflaredImage = "ghcr.io/matheuscscp/cloudflare-gateway-controller/cloudflared:2026.2.0@sha256:404528c1cd63c3eb882c257ae524919e4376115e6fe57befca8d603656a91a4c"

// GatewayReconciler reconciles Gateway objects.
type GatewayReconciler struct {
	client.Client
	NewTunnelClient  cfclient.TunnelClientFactory
	CloudflaredImage string
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;get;list;watch;update;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var gw gatewayv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var gc gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gc.Spec.ControllerName != gatewayv1.GatewayController(apiv1.ControllerName) {
		return ctrl.Result{}, nil
	}

	// Prune managed resources if the object is under deletion.
	if !gw.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &gw, &gc)
	}

	// Add finalizer first if it doesn't exist to avoid the race condition
	// between init and delete.
	if !controllerutil.ContainsFinalizer(&gw, apiv1.FinalizerGateway) {
		gwPatch := client.MergeFrom(gw.DeepCopy())
		controllerutil.AddFinalizer(&gw, apiv1.FinalizerGateway)
		if err := r.Patch(ctx, &gw, gwPatch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1}, nil
	}

	// Skip reconciliation if the object is suspended.
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		log.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling Gateway")

	// Ensure GatewayClass finalizer
	if err := r.ensureGatewayClassFinalizer(ctx, &gc); err != nil {
		return ctrl.Result{}, err
	}

	// Read credentials
	cfg, err := r.readCredentials(ctx, &gc, &gw)
	if err != nil {
		now := metav1.Now()
		credMsg := fmt.Sprintf("Failed to read credentials: %v", err)
		conditions := []*acmetav1.ConditionApplyConfiguration{
			acmetav1.Condition().
				WithType(string(gatewayv1.GatewayConditionAccepted)).
				WithStatus(metav1.ConditionFalse).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.GatewayReasonInvalidParameters)).
				WithMessage(credMsg),
			acmetav1.Condition().
				WithType(apiv1.ReadyCondition).
				WithStatus(metav1.ConditionFalse).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(apiv1.InvalidParametersNotReady).
				WithMessage(credMsg),
		}
		if existingTunnelID := tunnelIDFromStatus(&gw); existingTunnelID != "" {
			conditions = append(conditions, acmetav1.Condition().
				WithType(apiv1.ConditionTunnelID).
				WithStatus(metav1.ConditionTrue).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(apiv1.TunnelIDCreated).
				WithMessage(existingTunnelID),
			)
		}
		statusPatch := acgatewayv1.Gateway(gw.Name, gw.Namespace).
			WithStatus(acgatewayv1.GatewayStatus().
				WithConditions(conditions...).
				WithListeners(buildListenerStatusPatches(&gw)...),
			)
		if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Create tunnel client
	tc, err := r.NewTunnelClient(cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating tunnel client: %w", err)
	}

	// Resolve desired tunnel name
	tunnelName := gw.Annotations[apiv1.AnnotationTunnelName]
	if tunnelName == "" {
		tunnelName = string(gw.UID)
	}

	// Create or update tunnel
	tunnelID := tunnelIDFromStatus(&gw)
	if tunnelID == "" {
		tunnelID, err = tc.CreateTunnel(ctx, tunnelName)
		if err != nil {
			// A conflict is safe to ignore only when the tunnel name
			// defaults to the gateway UID, which is globally unique.
			if !cfclient.IsConflict(err) || tunnelName != string(gw.UID) {
				return ctrl.Result{}, fmt.Errorf("creating tunnel: %w", err)
			}
			tunnelID, err = tc.GetTunnelIDByName(ctx, tunnelName)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("looking up existing tunnel: %w", err)
			}
		}
		// Persist tunnel ID immediately to avoid leaking tunnels on crash.
		now := metav1.Now()
		statusPatch := acgatewayv1.Gateway(gw.Name, gw.Namespace).
			WithStatus(acgatewayv1.GatewayStatus().
				WithConditions(
					acmetav1.Condition().
						WithType(apiv1.ConditionTunnelID).
						WithStatus(metav1.ConditionTrue).
						WithObservedGeneration(gw.Generation).
						WithLastTransitionTime(now).
						WithReason(apiv1.TunnelIDCreated).
						WithMessage(tunnelID),
				),
			)
		if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("persisting tunnel ID in status: %w", err)
		}
	} else {
		currentName, err := tc.GetTunnelName(ctx, tunnelID)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting tunnel name: %w", err)
		}
		if currentName != tunnelName {
			if err := tc.UpdateTunnel(ctx, tunnelID, tunnelName); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating tunnel name: %w", err)
			}
		}
	}

	// Get tunnel token
	tunnelToken, err := tc.GetTunnelToken(ctx, tunnelID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting tunnel token: %w", err)
	}

	// Build and create/update tunnel token Secret
	secret := buildTunnelTokenSecret(&gw, tunnelToken)
	if err := controllerutil.SetControllerReference(&gw, secret, r.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference on secret: %w", err)
	}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Data = buildTunnelTokenSecret(&gw, tunnelToken).Data
		return controllerutil.SetControllerReference(&gw, secret, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating/updating tunnel token secret: %w", err)
	}
	log.V(1).Info("Reconciled tunnel token Secret", "result", result)

	// Parse replicas annotation
	var replicas *int32
	if v, ok := gw.Annotations[apiv1.AnnotationReplicas]; ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err == nil {
			replicas = new(int32(n))
		}
	}

	// Build and create/update cloudflared Deployment
	deploy := r.buildCloudflaredDeployment(&gw)
	if err := controllerutil.SetControllerReference(&gw, deploy, r.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
	}
	result, err = controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		currentReplicas := deploy.Spec.Replicas
		deploy.Spec = r.buildCloudflaredDeployment(&gw).Spec
		if replicas != nil {
			deploy.Spec.Replicas = replicas
		} else if currentReplicas != nil {
			deploy.Spec.Replicas = currentReplicas
		} else {
			deploy.Spec.Replicas = new(int32(1))
		}
		return controllerutil.SetControllerReference(&gw, deploy, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating/updating cloudflared deployment: %w", err)
	}
	log.V(1).Info("Reconciled cloudflared Deployment", "result", result)

	// Check Deployment readiness (deploy is already populated by CreateOrUpdate)
	deployReady := false
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == "True" {
			deployReady = true
			break
		}
	}

	// Apply status
	now := metav1.Now()
	programmedStatus := metav1.ConditionFalse
	programmedReason := string(gatewayv1.GatewayReasonPending)
	programmedMsg := "Waiting for cloudflared deployment to become ready"
	readyStatus := metav1.ConditionFalse
	readyReason := apiv1.NotReadyReason
	readyMsg := "Waiting for cloudflared deployment to become ready"
	if deployReady {
		programmedStatus = metav1.ConditionTrue
		programmedReason = string(gatewayv1.GatewayReasonProgrammed)
		programmedMsg = "Gateway is programmed"
		readyStatus = metav1.ConditionTrue
		readyReason = apiv1.ReadyReason
		readyMsg = "Gateway is ready"
	}
	statusPatch := acgatewayv1.Gateway(gw.Name, gw.Namespace).
		WithResourceVersion(gw.ResourceVersion).
		WithStatus(acgatewayv1.GatewayStatus().
			WithConditions(
				acmetav1.Condition().
					WithType(string(gatewayv1.GatewayConditionAccepted)).
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(string(gatewayv1.GatewayReasonAccepted)).
					WithMessage("Gateway is accepted"),
				acmetav1.Condition().
					WithType(string(gatewayv1.GatewayConditionProgrammed)).
					WithStatus(programmedStatus).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(programmedReason).
					WithMessage(programmedMsg),
				acmetav1.Condition().
					WithType(apiv1.ReadyCondition).
					WithStatus(readyStatus).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(readyReason).
					WithMessage(readyMsg),
				acmetav1.Condition().
					WithType(apiv1.ConditionTunnelID).
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(apiv1.TunnelIDCreated).
					WithMessage(tunnelID),
			).
			WithListeners(buildListenerStatusPatches(&gw)...),
		)
	if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: apiv1.ReconcileInterval(gw.Annotations)}, nil
}

func (r *GatewayReconciler) finalize(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(gw, apiv1.FinalizerGateway) {
		return ctrl.Result{}, nil
	}

	// Delete managed resources if reconciliation is not disabled.
	// When disabled, the user is responsible for manually cleaning up.
	if gw.Annotations[apiv1.AnnotationReconcile] != apiv1.ValueDisabled {
		// Delete the cloudflared Deployment and wait for it to be gone
		// before deleting the tunnel, so there are no active connections.
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cloudflaredDeploymentName(gw),
				Namespace: gw.Namespace,
			},
		}
		if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("deleting cloudflared deployment: %w", err)
		}
		deployKey := client.ObjectKeyFromObject(deploy)
		for {
			if err := r.Get(ctx, deployKey, deploy); apierrors.IsNotFound(err) {
				break
			} else if err != nil {
				return ctrl.Result{}, fmt.Errorf("waiting for cloudflared deployment deletion: %w", err)
			}
			select {
			case <-ctx.Done():
				return ctrl.Result{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}

		if tunnelID := tunnelIDFromStatus(gw); tunnelID != "" {
			cfg, err := r.readCredentials(ctx, gc, gw)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("reading credentials for tunnel deletion: %w", err)
			}
			tc, err := r.NewTunnelClient(cfg)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("creating tunnel client for deletion: %w", err)
			}
			if err := tc.DeleteTunnel(ctx, tunnelID); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting tunnel: %w", err)
			}
		}
	}

	// Remove finalizer from Gateway
	gwPatch := client.MergeFrom(gw.DeepCopy())
	controllerutil.RemoveFinalizer(gw, apiv1.FinalizerGateway)
	if err := r.Patch(ctx, gw, gwPatch); err != nil {
		return ctrl.Result{}, err
	}

	// If no other Gateways reference this GatewayClass, remove its finalizer
	if err := r.removeGatewayClassFinalizer(ctx, gc, gw); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) ensureGatewayClassFinalizer(ctx context.Context, gc *gatewayv1.GatewayClass) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Name: gc.Name}, gc); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(gc, apiv1.FinalizerGatewayClass) {
			return nil
		}
		gcPatch := client.MergeFromWithOptions(gc.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(gc, apiv1.FinalizerGatewayClass)
		return r.Patch(ctx, gc, gcPatch)
	})
}

func (r *GatewayReconciler) removeGatewayClassFinalizer(ctx context.Context, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var gwList gatewayv1.GatewayList
		if err := r.List(ctx, &gwList); err != nil {
			return err
		}
		for i := range gwList.Items {
			item := &gwList.Items[i]
			if item.UID != gw.UID && item.DeletionTimestamp.IsZero() && string(item.Spec.GatewayClassName) == gc.Name {
				return nil
			}
		}
		if err := r.Get(ctx, types.NamespacedName{Name: gc.Name}, gc); err != nil {
			return err
		}
		if !controllerutil.ContainsFinalizer(gc, apiv1.FinalizerGatewayClass) {
			return nil
		}
		gcPatch := client.MergeFromWithOptions(gc.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.RemoveFinalizer(gc, apiv1.FinalizerGatewayClass)
		return r.Patch(ctx, gc, gcPatch)
	})
}

func (r *GatewayReconciler) readCredentials(ctx context.Context, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway) (cfclient.ClientConfig, error) {
	if gc.Spec.ParametersRef == nil {
		return cfclient.ClientConfig{}, fmt.Errorf("gatewayclass %q has no parametersRef", gc.Name)
	}
	ref := gc.Spec.ParametersRef
	if string(ref.Kind) != "Secret" || (ref.Group != "" && ref.Group != "core" && ref.Group != gatewayv1.Group("")) {
		return cfclient.ClientConfig{}, fmt.Errorf("parametersRef must reference a core/v1 Secret")
	}
	if ref.Namespace == nil {
		return cfclient.ClientConfig{}, fmt.Errorf("parametersRef must specify a namespace")
	}

	secretNamespace := string(*ref.Namespace)
	secretName := ref.Name

	if granted, err := r.referenceGranted(ctx, gw.Namespace, secretNamespace, secretName); err != nil {
		return cfclient.ClientConfig{}, fmt.Errorf("checking ReferenceGrant: %w", err)
	} else if !granted {
		return cfclient.ClientConfig{}, fmt.Errorf("cross-namespace reference to Secret %s/%s not allowed by any ReferenceGrant", secretNamespace, secretName)
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: secretNamespace,
		Name:      secretName,
	}, &secret); err != nil {
		return cfclient.ClientConfig{}, fmt.Errorf("getting secret %s/%s: %w", secretNamespace, secretName, err)
	}

	apiToken := string(secret.Data["CLOUDFLARE_API_TOKEN"])
	accountID := string(secret.Data["CLOUDFLARE_ACCOUNT_ID"])
	if apiToken == "" || accountID == "" {
		return cfclient.ClientConfig{}, fmt.Errorf("secret %s/%s must contain CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID", secretNamespace, secretName)
	}

	return cfclient.ClientConfig{
		APIToken:  apiToken,
		AccountID: accountID,
	}, nil
}

func (r *GatewayReconciler) referenceGranted(ctx context.Context, gatewayNamespace, secretNamespace, secretName string) (bool, error) {
	if gatewayNamespace == secretNamespace {
		return true, nil
	}

	var grants gatewayv1beta1.ReferenceGrantList
	if err := r.List(ctx, &grants, client.InNamespace(secretNamespace)); err != nil {
		return false, fmt.Errorf("listing ReferenceGrants in namespace %s: %w", secretNamespace, err)
	}

	for i := range grants.Items {
		grant := &grants.Items[i]
		fromMatch := false
		for _, from := range grant.Spec.From {
			if from.Group == gatewayv1beta1.Group("gateway.networking.k8s.io") &&
				from.Kind == gatewayv1beta1.Kind("Gateway") &&
				string(from.Namespace) == gatewayNamespace {
				fromMatch = true
				break
			}
		}
		if !fromMatch {
			continue
		}
		for _, to := range grant.Spec.To {
			if to.Group == gatewayv1beta1.Group("") && to.Kind == gatewayv1beta1.Kind("Secret") {
				if to.Name == nil || string(*to.Name) == secretName {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

func tunnelIDFromStatus(gw *gatewayv1.Gateway) string {
	for _, c := range gw.Status.Conditions {
		if c.Type == apiv1.ConditionTunnelID && c.Reason == apiv1.TunnelIDCreated {
			return c.Message
		}
	}
	return ""
}

func buildListenerStatusPatches(gw *gatewayv1.Gateway) []*acgatewayv1.ListenerStatusApplyConfiguration {
	now := metav1.Now()
	patches := make([]*acgatewayv1.ListenerStatusApplyConfiguration, 0, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		supported := l.Protocol == gatewayv1.HTTPProtocolType || l.Protocol == gatewayv1.HTTPSProtocolType

		var acceptedCond, programmedCond *acmetav1.ConditionApplyConfiguration
		if supported {
			acceptedCond = acmetav1.Condition().
				WithType(string(gatewayv1.ListenerConditionAccepted)).
				WithStatus(metav1.ConditionTrue).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.ListenerReasonAccepted)).
				WithMessage("Listener is accepted")
			programmedCond = acmetav1.Condition().
				WithType(string(gatewayv1.ListenerConditionProgrammed)).
				WithStatus(metav1.ConditionTrue).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.ListenerReasonProgrammed)).
				WithMessage("Listener is programmed")
		} else {
			acceptedCond = acmetav1.Condition().
				WithType(string(gatewayv1.ListenerConditionAccepted)).
				WithStatus(metav1.ConditionFalse).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.ListenerReasonUnsupportedProtocol)).
				WithMessage(fmt.Sprintf("Protocol %q is not supported", l.Protocol))
			programmedCond = acmetav1.Condition().
				WithType(string(gatewayv1.ListenerConditionProgrammed)).
				WithStatus(metav1.ConditionFalse).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.ListenerReasonInvalid)).
				WithMessage("Listener is not programmed due to unsupported protocol")
		}

		ls := acgatewayv1.ListenerStatus().
			WithName(l.Name).
			WithSupportedKinds(
				acgatewayv1.RouteGroupKind().
					WithGroup(gatewayv1.Group(gatewayv1.GroupVersion.Group)).
					WithKind("HTTPRoute"),
			).
			WithAttachedRoutes(0).
			WithConditions(
				acceptedCond,
				programmedCond,
				acmetav1.Condition().
					WithType(string(gatewayv1.ListenerConditionConflicted)).
					WithStatus(metav1.ConditionFalse).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(string(gatewayv1.ListenerReasonNoConflicts)).
					WithMessage("No conflicts"),
				acmetav1.Condition().
					WithType(string(gatewayv1.ListenerConditionResolvedRefs)).
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(string(gatewayv1.ListenerReasonResolvedRefs)).
					WithMessage("References resolved"),
			)

		patches = append(patches, ls)
	}
	return patches
}

func cloudflaredDeploymentName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("cloudflared-%s", gw.Name)
}

func tunnelTokenSecretName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("cloudflared-token-%s", gw.Name)
}

func buildTunnelTokenSecret(gw *gatewayv1.Gateway, tunnelToken string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tunnelTokenSecretName(gw),
			Namespace: gw.Namespace,
		},
		Data: map[string][]byte{
			"TUNNEL_TOKEN": []byte(tunnelToken),
		},
	}
}

func (r *GatewayReconciler) buildCloudflaredDeployment(gw *gatewayv1.Gateway) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": "cloudflare-gateway-controller",
		"app.kubernetes.io/instance":   gw.Name,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudflaredDeploymentName(gw),
			Namespace: gw.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "cloudflared",
							Image: r.CloudflaredImage,
							Args:  []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"},
							Env: []corev1.EnvVar{
								{
									Name: "TUNNEL_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: tunnelTokenSecretName(gw),
											},
											Key: "TUNNEL_TOKEN",
										},
									},
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/ready",
										Port: intstr.FromInt32(2000),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
