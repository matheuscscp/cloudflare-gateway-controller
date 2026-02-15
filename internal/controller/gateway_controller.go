// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// 1. Fetch Gateway
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Fetch GatewayClass
	var gc gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gc.Spec.ControllerName != gatewayv1.GatewayController(apiv1.ControllerName) {
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling Gateway")

	// 3. Deletion path
	if gw.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(&gw, apiv1.GatewayFinalizer) {
			return ctrl.Result{}, nil
		}

		// Delete tunnel if annotation exists
		if tunnelID := gw.Annotations[apiv1.TunnelIDAnnotation]; tunnelID != "" {
			cfg, err := r.readCredentials(ctx, &gc)
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

		// Remove finalizer from Gateway
		controllerutil.RemoveFinalizer(&gw, apiv1.GatewayFinalizer)
		if err := r.Update(ctx, &gw); err != nil {
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
		if !hasOtherGateways && controllerutil.ContainsFinalizer(&gc, gatewayv1.GatewayClassFinalizerGatewaysExist) {
			controllerutil.RemoveFinalizer(&gc, gatewayv1.GatewayClassFinalizerGatewaysExist)
			if err := r.Update(ctx, &gc); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	// 4. Normal path

	// Ensure finalizer on Gateway
	if controllerutil.AddFinalizer(&gw, apiv1.GatewayFinalizer) {
		if err := r.Update(ctx, &gw); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Ensure GatewayClass finalizer
	if controllerutil.AddFinalizer(&gc, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		if err := r.Update(ctx, &gc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Validate listeners
	listenerStatuses := validateListeners(&gw)

	// Read credentials
	cfg, err := r.readCredentials(ctx, &gc)
	if err != nil {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             string(gatewayv1.GatewayReasonInvalidParameters),
			Message:            fmt.Sprintf("Failed to read credentials: %v", err),
		})
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               apiv1.ReadyCondition,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             apiv1.InvalidParametersNotReady,
			Message:            fmt.Sprintf("Failed to read credentials: %v", err),
		})
		gw.Status.Listeners = listenerStatuses
		if err := r.Status().Update(ctx, &gw); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Set Accepted=True
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            "Gateway is accepted",
	})

	// Create tunnel client
	tc, err := r.NewTunnelClient(cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating tunnel client: %w", err)
	}

	// Create tunnel if not yet created
	if gw.Annotations == nil {
		gw.Annotations = make(map[string]string)
	}
	if gw.Annotations[apiv1.TunnelIDAnnotation] == "" {
		tunnelID, err := tc.CreateTunnel(ctx, fmt.Sprintf("%s-%s", gw.Namespace, gw.Name))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("creating tunnel: %w", err)
		}
		gw.Annotations[apiv1.TunnelIDAnnotation] = tunnelID
		if err := r.Update(ctx, &gw); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get tunnel token
	tunnelToken, err := tc.GetTunnelToken(ctx, gw.Annotations[apiv1.TunnelIDAnnotation])
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting tunnel token: %w", err)
	}

	// Build and create/update cloudflared Deployment
	deploy := buildCloudflaredDeployment(&gw, tunnelToken, r.CloudflaredImage)
	if err := controllerutil.SetControllerReference(&gw, deploy, r.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
	}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		// Update the spec on existing deployment
		deploy.Spec = buildCloudflaredDeployment(&gw, tunnelToken, r.CloudflaredImage).Spec
		return controllerutil.SetControllerReference(&gw, deploy, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating/updating cloudflared deployment: %w", err)
	}
	log.V(1).Info("Reconciled cloudflared Deployment", "result", result)

	// Set Programmed=True
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             string(gatewayv1.GatewayReasonProgrammed),
		Message:            "Gateway is programmed",
	})

	// Set Ready=True (kstatus)
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               apiv1.ReadyCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             apiv1.ReadyReason,
		Message:            "Gateway is ready",
	})

	gw.Status.Listeners = listenerStatuses
	if err := r.Status().Update(ctx, &gw); err != nil {
		return ctrl.Result{}, err
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

func validateListeners(gw *gatewayv1.Gateway) []gatewayv1.ListenerStatus {
	statuses := make([]gatewayv1.ListenerStatus, 0, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		supported := l.Protocol == gatewayv1.HTTPProtocolType || l.Protocol == gatewayv1.HTTPSProtocolType

		ls := gatewayv1.ListenerStatus{
			Name: l.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: (*gatewayv1.Group)(&gatewayv1.GroupVersion.Group),
					Kind:  "HTTPRoute",
				},
			},
			AttachedRoutes: 0,
		}

		if supported {
			meta.SetStatusCondition(&ls.Conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gw.Generation,
				Reason:             string(gatewayv1.ListenerReasonAccepted),
				Message:            "Listener is accepted",
			})
			meta.SetStatusCondition(&ls.Conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionProgrammed),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gw.Generation,
				Reason:             string(gatewayv1.ListenerReasonProgrammed),
				Message:            "Listener is programmed",
			})
		} else {
			meta.SetStatusCondition(&ls.Conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				Reason:             string(gatewayv1.ListenerReasonUnsupportedProtocol),
				Message:            fmt.Sprintf("Protocol %q is not supported", l.Protocol),
			})
			meta.SetStatusCondition(&ls.Conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionProgrammed),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				Reason:             string(gatewayv1.ListenerReasonInvalid),
				Message:            "Listener is not programmed due to unsupported protocol",
			})
		}

		meta.SetStatusCondition(&ls.Conditions, metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionConflicted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             string(gatewayv1.ListenerReasonNoConflicts),
			Message:            "No conflicts",
		})
		meta.SetStatusCondition(&ls.Conditions, metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
			Message:            "References resolved",
		})

		statuses = append(statuses, ls)
	}
	return statuses
}

func buildCloudflaredDeployment(gw *gatewayv1.Gateway, tunnelToken, cloudflaredImage string) *appsv1.Deployment {
	replicas := int32(1)
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
			Replicas: &replicas,
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
							Image: cloudflaredImage,
							Args:  []string{"tunnel", "--no-autoupdate", "run"},
							Env: []corev1.EnvVar{
								{
									Name:  "TUNNEL_TOKEN",
									Value: tunnelToken,
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
