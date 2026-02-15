// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/fluxcd/cli-utils/pkg/object"
	"github.com/fluxcd/pkg/ssa"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
	ResourceManager  *ssa.ResourceManager
	NewTunnelClient  cfclient.TunnelClientFactory
	CloudflaredImage string
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;get;list;watch;update;delete
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
		gwPatch := client.MergeFromWithOptions(gw.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(&gw, apiv1.FinalizerGateway)
		if err := r.Patch(ctx, &gw, gwPatch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Skip reconciliation if the object is suspended.
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		log.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling Gateway")

	// Ensure GatewayClass finalizer
	if !controllerutil.ContainsFinalizer(&gc, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		gcPatch := client.MergeFromWithOptions(gc.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(&gc, gatewayv1.GatewayClassFinalizerGatewaysExist)
		if err := r.Patch(ctx, &gc, gcPatch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Read credentials
	cfg, err := r.readCredentials(ctx, &gc)
	if err != nil {
		now := metav1.Now()
		credMsg := fmt.Sprintf("Failed to read credentials: %v", err)
		statusPatch := acgatewayv1.Gateway(gw.Name, gw.Namespace).
			WithResourceVersion(gw.ResourceVersion).
			WithStatus(acgatewayv1.GatewayStatus().
				WithConditions(
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
				).
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

	// Create tunnel if not yet created
	if gw.Annotations == nil {
		gw.Annotations = make(map[string]string)
	}
	if gw.Annotations[apiv1.AnnotationTunnelID] == "" {
		tunnelID, err := tc.CreateTunnel(ctx, fmt.Sprintf("%s-%s", gw.Namespace, gw.Name))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("creating tunnel: %w", err)
		}
		annPatch := client.MergeFromWithOptions(gw.DeepCopy(), client.MergeFromWithOptimisticLock{})
		gw.Annotations[apiv1.AnnotationTunnelID] = tunnelID
		if err := r.Patch(ctx, &gw, annPatch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get tunnel token
	tunnelToken, err := tc.GetTunnelToken(ctx, gw.Annotations[apiv1.AnnotationTunnelID])
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

	// Wait for Deployment to become ready
	if err := r.ResourceManager.WaitForSetWithContext(ctx, object.ObjMetadataSet{
		{
			Namespace: deploy.Namespace,
			Name:      deploy.Name,
			GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
		},
	}, ssa.WaitOptions{
		Interval: 5 * time.Second,
		Timeout:  apiv1.ReconcileTimeout(gw.Annotations),
		FailFast: true,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("waiting for cloudflared deployment to become ready: %w", err)
	}

	// Apply status
	now := metav1.Now()
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
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(string(gatewayv1.GatewayReasonProgrammed)).
					WithMessage("Gateway is programmed"),
				acmetav1.Condition().
					WithType(apiv1.ReadyCondition).
					WithStatus(metav1.ConditionTrue).
					WithObservedGeneration(gw.Generation).
					WithLastTransitionTime(now).
					WithReason(apiv1.ReadyReason).
					WithMessage("Gateway is ready"),
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

	// Delete tunnel if annotation exists and reconciliation is not disabled.
	// When disabled, the user is responsible for manually cleaning up the tunnel.
	if gw.Annotations[apiv1.AnnotationReconcile] != apiv1.ValueDisabled {
		if tunnelID := gw.Annotations[apiv1.AnnotationTunnelID]; tunnelID != "" {
			cfg, err := r.readCredentials(ctx, gc)
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
	gwPatch := client.MergeFromWithOptions(gw.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(gw, apiv1.FinalizerGateway)
	if err := r.Patch(ctx, gw, gwPatch); err != nil {
		return ctrl.Result{}, err
	}

	// If no other Gateways reference this GatewayClass, remove its finalizer
	var gwList gatewayv1.GatewayList
	if err := r.List(ctx, &gwList); err != nil {
		return ctrl.Result{}, err
	}
	hasOtherGateways := false
	for i := range gwList.Items {
		if gwList.Items[i].UID != gw.UID && string(gwList.Items[i].Spec.GatewayClassName) == gc.Name {
			hasOtherGateways = true
			break
		}
	}
	if !hasOtherGateways && controllerutil.ContainsFinalizer(gc, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		gcPatch := client.MergeFromWithOptions(gc.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.RemoveFinalizer(gc, gatewayv1.GatewayClassFinalizerGatewaysExist)
		if err := r.Patch(ctx, gc, gcPatch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) readCredentials(ctx context.Context, gc *gatewayv1.GatewayClass) (cfclient.ClientConfig, error) {
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

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: string(*ref.Namespace),
		Name:      ref.Name,
	}, &secret); err != nil {
		return cfclient.ClientConfig{}, fmt.Errorf("getting secret %s/%s: %w", *ref.Namespace, ref.Name, err)
	}

	apiToken := string(secret.Data["CLOUDFLARE_API_TOKEN"])
	accountID := string(secret.Data["CLOUDFLARE_ACCOUNT_ID"])
	if apiToken == "" || accountID == "" {
		return cfclient.ClientConfig{}, fmt.Errorf("secret %s/%s must contain CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID", *ref.Namespace, ref.Name)
	}

	return cfclient.ClientConfig{
		APIToken:  apiToken,
		AccountID: accountID,
	}, nil
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
			Name:      fmt.Sprintf("cloudflared-%s", gw.Name),
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
							Args:  []string{"tunnel", "--no-autoupdate", "run"},
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
