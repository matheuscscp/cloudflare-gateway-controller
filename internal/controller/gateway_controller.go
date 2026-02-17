// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	acappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	accorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
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
	events.EventRecorder
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
		result, err := r.finalize(ctx, &gw, &gc)
		if err != nil {
			r.Eventf(&gw, nil, corev1.EventTypeWarning, apiv1.ReasonFailed, "Finalize", "Finalization failed: %v", err)
			return ctrl.Result{}, err
		}
		return result, nil
	}

	// Add finalizer first if it doesn't exist to avoid the race condition
	// between init and delete.
	if !controllerutil.ContainsFinalizer(&gw, apiv1.Finalizer) {
		gwPatch := client.MergeFrom(gw.DeepCopy())
		controllerutil.AddFinalizer(&gw, apiv1.Finalizer)
		if err := r.Patch(ctx, &gw, gwPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: 1}, nil
	}

	// Skip reconciliation if the object is suspended.
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		log.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Reconciling Gateway")

	return r.reconcile(ctx, &gw, &gc)
}

func (r *GatewayReconciler) reconcile(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Count attached HTTPRoutes per listener.
	attachedRoutes, err := countAttachedRoutes(ctx, r.Client, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("counting attached routes: %w", err))
	}

	// Ensure GatewayClass finalizer
	if err := r.ensureGatewayClassFinalizer(ctx, gc); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("ensuring GatewayClass finalizer: %w", err))
	}

	listenerPatches := buildListenerStatusPatches(gw, attachedRoutes)
	now := metav1.Now()
	readyStatus := metav1.ConditionFalse
	readyReason := apiv1.ReasonFailed
	readyMsg := ""
	var conditions []*acmetav1.ConditionApplyConfiguration

	// Read credentials
	cfg, err := readCredentials(ctx, r.Client, gc, gw)
	if err != nil {
		credMsg := fmt.Sprintf("Failed to read credentials: %v", err)
		readyReason = apiv1.ReasonInvalidParams
		readyMsg = credMsg
		conditions = []*acmetav1.ConditionApplyConfiguration{
			acmetav1.Condition().
				WithType(string(gatewayv1.GatewayConditionAccepted)).
				WithStatus(metav1.ConditionFalse).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(string(gatewayv1.GatewayReasonInvalidParameters)).
				WithMessage(credMsg),
			acmetav1.Condition().
				WithType(apiv1.ConditionReady).
				WithStatus(metav1.ConditionFalse).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(apiv1.ReasonInvalidParams).
				WithMessage(credMsg),
		}
	} else {
		// Create tunnel client
		tc, err := r.NewTunnelClient(cfg)
		if err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("creating tunnel client: %w", err))
		}

		// Look up or create tunnel. The name is deterministic (gateway-{UID}),
		// so Cloudflare is the source of truth — no local state tracking needed.
		name := apiv1.TunnelName(gw)
		tunnelID, err := tc.GetTunnelIDByName(ctx, name)
		if err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("looking up tunnel: %w", err))
		}
		if tunnelID == "" {
			tunnelID, err = tc.CreateTunnel(ctx, name)
			if err != nil {
				if !cfclient.IsConflict(err) {
					return r.reconcileError(ctx, gw, fmt.Errorf("creating tunnel: %w", err))
				}
				tunnelID, err = tc.GetTunnelIDByName(ctx, name)
				if err != nil {
					return r.reconcileError(ctx, gw, fmt.Errorf("looking up tunnel after conflict: %w", err))
				}
			}
		}

		// Get tunnel token
		tunnelToken, err := tc.GetTunnelToken(ctx, tunnelID)
		if err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("getting tunnel token: %w", err))
		}

		// Build and create/update tunnel token Secret
		secret := buildTunnelTokenSecret(gw, tunnelToken)
		if err := controllerutil.SetControllerReference(gw, secret, r.Scheme()); err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("setting owner reference on secret: %w", err))
		}
		secretResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			desired := buildTunnelTokenSecret(gw, tunnelToken)
			secret.Data = desired.Data
			secret.Labels = desired.Labels
			secret.Annotations = desired.Annotations
			return controllerutil.SetControllerReference(gw, secret, r.Scheme())
		})
		if err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("creating/updating tunnel token secret: %w", err))
		}
		log.V(1).Info("Reconciled tunnel token Secret", "result", secretResult)

		// Parse replicas annotation
		var replicas *int32
		if v, ok := gw.Annotations[apiv1.AnnotationReplicas]; ok {
			n, err := strconv.ParseInt(v, 10, 32)
			if err == nil {
				replicas = new(int32(n))
			}
		}

		// Apply cloudflared Deployment via Server-Side Apply. Only the fields we
		// declare are managed; Kubernetes-defaulted fields (strategy, replicas when
		// unset, container defaults, etc.) are left untouched, avoiding spurious
		// updates that would trigger a watch loop.
		deployApply := r.buildCloudflaredDeploymentApply(gw, replicas)
		if err := r.Apply(ctx, deployApply, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("applying cloudflared deployment: %w", err))
		}
		log.V(1).Info("Reconciled cloudflared Deployment")

		// List all HTTPRoutes for this Gateway and update tunnel ingress configuration.
		gatewayRoutes, deniedRoutes, err := listGatewayRoutes(ctx, r.Client, gw)
		if err != nil {
			return r.reconcileError(ctx, gw, err)
		}
		if len(deniedRoutes) > 0 {
			log.Info("HTTPRoutes denied due to missing or failed ReferenceGrant checks", "count", len(deniedRoutes))
		}
		ingress, deniedBackendRefs, err := buildIngressRules(ctx, r.Client, gatewayRoutes)
		if err != nil {
			return r.reconcileError(ctx, gw, err)
		}
		if len(deniedBackendRefs) > 0 {
			log.Info("BackendRefs denied due to missing or failed ReferenceGrant checks", "count", len(deniedBackendRefs))
		}
		if err := tc.UpdateTunnelConfiguration(ctx, tunnelID, ingress); err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("updating tunnel configuration: %w", err))
		}

		// Reconcile DNS CNAME records. The condition captures success, errors,
		// or "not enabled" state.
		dnsCond := r.reconcileDNS(ctx, tc, tunnelID, gw.Annotations[apiv1.AnnotationZoneName], gw.Generation, gw.Status.Conditions, gatewayRoutes)

		// Check Deployment readiness.
		var deploy appsv1.Deployment
		deployReady := false
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: gw.Namespace,
			Name:      apiv1.CloudflaredDeploymentName(gw),
		}, &deploy); err == nil {
			for _, c := range deploy.Status.Conditions {
				if c.Type == appsv1.DeploymentAvailable && c.Status == "True" {
					deployReady = true
					break
				}
			}
		}

		programmedStatus := metav1.ConditionFalse
		programmedReason := string(gatewayv1.GatewayReasonPending)
		programmedMsg := "Waiting for cloudflared deployment to become ready"
		if deployReady {
			programmedStatus = metav1.ConditionTrue
			programmedReason = string(gatewayv1.GatewayReasonProgrammed)
			programmedMsg = "Gateway is programmed"
			readyStatus = metav1.ConditionTrue
			readyReason = apiv1.ReasonReconciled
			readyMsg = "Gateway is ready"
		} else {
			readyMsg = "Waiting for cloudflared deployment to become ready"
		}
		conditions = []*acmetav1.ConditionApplyConfiguration{
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
				WithType(apiv1.ConditionReady).
				WithStatus(readyStatus).
				WithObservedGeneration(gw.Generation).
				WithLastTransitionTime(now).
				WithReason(readyReason).
				WithMessage(readyMsg),
		}
		conditions = append(conditions, routeReferenceGrantsCondition(gw.Generation, now, deniedRoutes))
		conditions = append(conditions, backendReferenceGrantsCondition(gw.Generation, now, deniedBackendRefs))
		conditions = append(conditions, dnsCond)
	}

	// Skip the status patch if no conditions or listener statuses changed.
	requeueAfter := apiv1.ReconcileInterval(gw.Annotations)
	changed := false
	for _, c := range conditions {
		if applyConditionChanged(gw.Status.Conditions, c, gw.Generation) {
			changed = true
			break
		}
	}
	if !changed && listenersChanged(gw.Status.Listeners, listenerPatches, gw.Generation) {
		changed = true
	}
	if !changed {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	statusPatch := acgatewayv1.Gateway(gw.Name, gw.Namespace).
		WithResourceVersion(gw.ResourceVersion).
		WithStatus(acgatewayv1.GatewayStatus().
			WithConditions(conditions...).
			WithListeners(listenerPatches...),
		)
	if err := r.Status().Apply(ctx, statusPatch, client.FieldOwner(apiv1.ControllerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	if readyStatus == metav1.ConditionFalse {
		r.Eventf(gw, nil, corev1.EventTypeWarning, readyReason, "Reconcile", readyMsg)
	} else {
		r.Eventf(gw, nil, corev1.EventTypeNormal, apiv1.ReasonReconciled, "Reconcile", "Gateway reconciled")
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileError handles business logic errors by best-effort patching Ready=Unknown
// on the Gateway status and emitting a Warning event. It uses a merge patch (not SSA)
// to update only the Ready condition without clobbering other conditions.
func (r *GatewayReconciler) reconcileError(ctx context.Context, gw *gatewayv1.Gateway, reconcileErr error) (ctrl.Result, error) {
	msg := reconcileErr.Error()
	if conditionChanged(gw.Status.Conditions, apiv1.ConditionReady, metav1.ConditionUnknown, apiv1.ReasonProgressingWithRetry, msg, gw.Generation) {
		patch := client.MergeFrom(gw.DeepCopy())
		now := metav1.Now()
		cond := findCondition(gw.Status.Conditions, apiv1.ConditionReady)
		if cond != nil {
			cond.Status = metav1.ConditionUnknown
			cond.Reason = apiv1.ReasonProgressingWithRetry
			cond.Message = msg
			cond.ObservedGeneration = gw.Generation
			cond.LastTransitionTime = now
		} else {
			gw.Status.Conditions = append(gw.Status.Conditions, metav1.Condition{
				Type:               apiv1.ConditionReady,
				Status:             metav1.ConditionUnknown,
				ObservedGeneration: gw.Generation,
				LastTransitionTime: now,
				Reason:             apiv1.ReasonProgressingWithRetry,
				Message:            msg,
			})
		}
		_ = r.Status().Patch(ctx, gw, patch)
	}
	r.Eventf(gw, nil, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, "Reconcile", "Reconciliation failed: %v", reconcileErr)
	return ctrl.Result{}, reconcileErr
}

// finalize cleans up managed resources (cloudflared Deployment and Cloudflare tunnel)
// before allowing the Gateway to be deleted, then removes the finalizer.
func (r *GatewayReconciler) finalize(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(gw, apiv1.Finalizer) {
		return ctrl.Result{}, nil
	}

	// When reconciliation is disabled, remove owner references from managed
	// resources so Kubernetes GC doesn't delete them when the Gateway is removed.
	// The user is responsible for manually cleaning up.
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		if err := r.removeOwnerReferences(ctx, gw); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing owner references: %w", err)
		}
	}

	// Delete managed resources if reconciliation is not disabled.
	if gw.Annotations[apiv1.AnnotationReconcile] != apiv1.ValueDisabled {
		// Delete the cloudflared Deployment and wait for it to be gone
		// before deleting the tunnel, so there are no active connections.
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      apiv1.CloudflaredDeploymentName(gw),
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

		cfg, err := readCredentials(ctx, r.Client, gc, gw)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("reading credentials for tunnel deletion: %w", err)
		}
		tc, err := r.NewTunnelClient(cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("creating tunnel client for deletion: %w", err)
		}
		tunnelID, err := tc.GetTunnelIDByName(ctx, apiv1.TunnelName(gw))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("looking up tunnel for deletion: %w", err)
		}
		if tunnelID != "" {
			// Delete DNS CNAME records across all zones.
			if err := r.cleanupAllDNS(ctx, tc, tunnelID); err != nil {
				return ctrl.Result{}, fmt.Errorf("cleaning up DNS during finalization: %w", err)
			}
			if err := tc.DeleteTunnel(ctx, tunnelID); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting tunnel: %w", err)
			}
		}
	}

	// Remove finalizer from Gateway
	gwPatch := client.MergeFrom(gw.DeepCopy())
	controllerutil.RemoveFinalizer(gw, apiv1.Finalizer)
	if err := r.Patch(ctx, gw, gwPatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	// If no other Gateways reference this GatewayClass, remove its finalizer
	if err := r.removeGatewayClassFinalizer(ctx, gc, gw); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing GatewayClass finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// removeOwnerReferences removes the Gateway's owner references from managed
// resources (cloudflared Deployment and tunnel token Secret) so they survive
// garbage collection when the Gateway is deleted with reconciliation disabled.
func (r *GatewayReconciler) removeOwnerReferences(ctx context.Context, gw *gatewayv1.Gateway) error {
	// Remove owner reference from cloudflared Deployment.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: gw.Namespace, Name: apiv1.CloudflaredDeploymentName(gw)}
	if err := r.Get(ctx, deployKey, &deploy); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting deployment: %w", err)
		}
	} else {
		deployPatch := client.MergeFrom(deploy.DeepCopy())
		if removeOwnerRef(&deploy, gw.UID) {
			if err := r.Patch(ctx, &deploy, deployPatch); err != nil {
				return fmt.Errorf("patching deployment: %w", err)
			}
		}
	}

	// Remove owner reference from tunnel token Secret.
	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: gw.Namespace, Name: apiv1.TunnelTokenSecretName(gw)}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting secret: %w", err)
		}
	} else {
		secretPatch := client.MergeFrom(secret.DeepCopy())
		if removeOwnerRef(&secret, gw.UID) {
			if err := r.Patch(ctx, &secret, secretPatch); err != nil {
				return fmt.Errorf("patching secret: %w", err)
			}
		}
	}

	return nil
}

// removeOwnerRef removes the owner reference with the given UID from the object.
// Returns true if an owner reference was removed.
func removeOwnerRef(obj client.Object, ownerUID types.UID) bool {
	refs := obj.GetOwnerReferences()
	for i, ref := range refs {
		if ref.UID == ownerUID {
			obj.SetOwnerReferences(append(refs[:i], refs[i+1:]...))
			return true
		}
	}
	return false
}

// reconcileDNS reconciles DNS CNAME records for the Gateway's zone and returns
// a DNSManagement condition reflecting the result. When zoneName is empty and
// DNS was previously enabled (condition was True), all CNAME records pointing
// to the tunnel across all account zones are deleted. DNS errors are captured
// in the condition rather than failing the whole reconciliation.
func (r *GatewayReconciler) reconcileDNS(ctx context.Context, tc cfclient.TunnelClient, tunnelID, zoneName string, generation int64, existingConditions []metav1.Condition, routes []*gatewayv1.HTTPRoute) *acmetav1.ConditionApplyConfiguration {
	log := log.FromContext(ctx)

	if zoneName == "" {
		// Only clean up if DNS was previously enabled (condition was True,
		// meaning records may exist). Skip API calls otherwise.
		prev := findCondition(existingConditions, apiv1.ConditionDNSManagement)
		if prev != nil && prev.Status == metav1.ConditionTrue {
			if err := r.cleanupAllDNS(ctx, tc, tunnelID); err != nil {
				return dnsCondition(metav1.ConditionFalse, generation, existingConditions,
					apiv1.ReasonFailed, fmt.Sprintf("Failed to clean up DNS records: %v", err))
			}
		}
		return dnsCondition(metav1.ConditionFalse, generation, existingConditions,
			apiv1.ReasonDNSNotEnabled, "DNS management is not enabled.\n\n"+
				"Set the "+apiv1.AnnotationZoneName+" annotation to a subdomain to enable.\n\n"+
				"For example, if the subdomain is example.com, the Gateway will create and reconcile CNAME records for\n\n"+
				"hostnames like myapp.example.com that point to the tunnel.")
	}

	zoneID, err := tc.FindZoneIDByHostname(ctx, zoneName)
	if err != nil {
		return dnsCondition(metav1.ConditionFalse, generation, existingConditions,
			apiv1.ReasonFailed, fmt.Sprintf("Failed to find zone ID for %q: %v", zoneName, err))
	}

	// Compute desired hostnames from all attached HTTPRoutes.
	desired := make(map[string]struct{})
	type skippedRoute struct {
		key       string
		hostnames []string
	}
	var skippedRoutes []skippedRoute
	for _, route := range routes {
		var routeSkipped []string
		for _, h := range route.Spec.Hostnames {
			hostname := string(h)
			if hostname != zoneName && !strings.HasSuffix(hostname, "."+zoneName) {
				routeSkipped = append(routeSkipped, hostname)
				continue
			}
			desired[hostname] = struct{}{}
		}
		if len(routeSkipped) > 0 {
			skippedRoutes = append(skippedRoutes, skippedRoute{
				key:       route.Namespace + "/" + route.Name,
				hostnames: routeSkipped,
			})
		}
	}

	// Query actual CNAME records pointing to our tunnel.
	tunnelTarget := tunnelID + ".cfargotunnel.com"
	actual, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
	if err != nil {
		return dnsCondition(metav1.ConditionFalse, generation, existingConditions,
			apiv1.ReasonFailed, fmt.Sprintf("Failed to list DNS CNAMEs: %v", err))
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, h := range actual {
		actualSet[h] = struct{}{}
	}

	// Create missing records.
	for h := range desired {
		if _, ok := actualSet[h]; !ok {
			if err := tc.EnsureDNSCNAME(ctx, zoneID, h, tunnelTarget); err != nil {
				return dnsCondition(metav1.ConditionFalse, generation, existingConditions,
					apiv1.ReasonFailed, fmt.Sprintf("Failed to ensure DNS CNAME for %q: %v", h, err))
			}
			log.V(1).Info("Created DNS CNAME", "hostname", h)
		}
	}

	// Delete stale records.
	for h := range actualSet {
		if _, ok := desired[h]; !ok {
			if err := tc.DeleteDNSCNAME(ctx, zoneID, h); err != nil {
				return dnsCondition(metav1.ConditionFalse, generation, existingConditions,
					apiv1.ReasonFailed, fmt.Sprintf("Failed to delete stale DNS CNAME for %q: %v", h, err))
			}
			log.V(1).Info("Deleted stale DNS CNAME", "hostname", h)
		}
	}

	var msg strings.Builder
	fmt.Fprintf(&msg, "DNS records reconciled for %d hostname(s)", len(desired))
	if len(skippedRoutes) > 0 {
		fmt.Fprintf(&msg, "\nHostnames not in zone %q:", zoneName)
		for _, s := range skippedRoutes {
			fmt.Fprintf(&msg, "\n- HTTPRoute %s: %s", s.key, strings.Join(s.hostnames, ", "))
		}
	}
	return dnsCondition(metav1.ConditionTrue, generation, existingConditions, apiv1.ReasonDNSReconciled, msg.String())
}

// dnsCondition builds a DNSManagement condition. If the existing condition has
// the same status, reason, and message, LastTransitionTime is preserved to avoid
// unnecessary status updates.
func dnsCondition(status metav1.ConditionStatus, generation int64, existingConditions []metav1.Condition, reason, message string) *acmetav1.ConditionApplyConfiguration {
	cond := acmetav1.Condition().
		WithType(apiv1.ConditionDNSManagement).
		WithStatus(status).
		WithObservedGeneration(generation).
		WithReason(reason).
		WithMessage(message)
	if prev := findCondition(existingConditions, apiv1.ConditionDNSManagement); prev != nil &&
		prev.Status == status && prev.Reason == reason && prev.Message == message {
		cond.WithLastTransitionTime(prev.LastTransitionTime)
	} else {
		cond.WithLastTransitionTime(metav1.Now())
	}
	return cond
}

// cleanupAllDNS deletes all CNAME records pointing to the tunnel across all
// account zones. This is used when the zoneName annotation is removed.
func (r *GatewayReconciler) cleanupAllDNS(ctx context.Context, tc cfclient.TunnelClient, tunnelID string) error {
	log := log.FromContext(ctx)
	tunnelTarget := tunnelID + ".cfargotunnel.com"

	zoneIDs, err := tc.ListZoneIDs(ctx)
	if err != nil {
		return fmt.Errorf("listing zones: %w", err)
	}

	for _, zoneID := range zoneIDs {
		hostnames, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
		if err != nil {
			return fmt.Errorf("listing DNS CNAMEs in zone %s: %w", zoneID, err)
		}
		for _, h := range hostnames {
			if err := tc.DeleteDNSCNAME(ctx, zoneID, h); err != nil {
				return fmt.Errorf("deleting DNS CNAME %q in zone %s: %w", h, zoneID, err)
			}
			log.V(1).Info("Deleted DNS CNAME (zoneName removed or object deleted)", "hostname", h, "zoneID", zoneID)
		}
	}

	return nil
}

// ensureGatewayClassFinalizer adds the GatewayClass finalizer if not already present,
// preventing the GatewayClass from being deleted while Gateways reference it.
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

// removeGatewayClassFinalizer removes the GatewayClass finalizer if no other
// non-deleting Gateways reference the class.
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

// readCredentials reads the Cloudflare API credentials from the Secret referenced
// by the Gateway's infrastructure parametersRef or the GatewayClass parametersRef.
// Cross-namespace references are validated against ReferenceGrants.
func readCredentials(ctx context.Context, r client.Reader, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway) (cfclient.ClientConfig, error) {
	var secretNamespace, secretName string

	if gw.Spec.Infrastructure != nil && gw.Spec.Infrastructure.ParametersRef != nil {
		ref := gw.Spec.Infrastructure.ParametersRef
		if string(ref.Kind) != apiv1.KindSecret || (ref.Group != "" && ref.Group != "core" && ref.Group != gatewayv1.Group("")) {
			return cfclient.ClientConfig{}, fmt.Errorf("infrastructure parametersRef must reference a core/v1 Secret")
		}
		secretNamespace = gw.Namespace
		secretName = string(ref.Name)
	} else {
		if gc.Spec.ParametersRef == nil {
			return cfclient.ClientConfig{}, fmt.Errorf("gatewayclass %q has no parametersRef", gc.Name)
		}
		ref := gc.Spec.ParametersRef
		if string(ref.Kind) != apiv1.KindSecret || (ref.Group != "" && ref.Group != "core" && ref.Group != gatewayv1.Group("")) {
			return cfclient.ClientConfig{}, fmt.Errorf("parametersRef must reference a core/v1 Secret")
		}
		if ref.Namespace == nil {
			return cfclient.ClientConfig{}, fmt.Errorf("parametersRef must specify a namespace")
		}
		secretNamespace = string(*ref.Namespace)
		secretName = ref.Name

		if granted, err := secretReferenceGranted(ctx, r, gw.Namespace, secretNamespace, secretName); err != nil {
			return cfclient.ClientConfig{}, fmt.Errorf("checking ReferenceGrant: %w", err)
		} else if !granted {
			return cfclient.ClientConfig{}, fmt.Errorf("cross-namespace reference to Secret %s/%s not allowed by any ReferenceGrant", secretNamespace, secretName)
		}
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

// buildListenerStatusPatches builds SSA apply configurations for each Gateway listener,
// setting Accepted/Programmed conditions based on protocol support and the attached route count.
func buildListenerStatusPatches(gw *gatewayv1.Gateway, attachedRoutes map[gatewayv1.SectionName]int32) []*acgatewayv1.ListenerStatusApplyConfiguration {
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
					WithKind(apiv1.KindHTTPRoute),
			).
			WithAttachedRoutes(attachedRoutes[l.Name]).
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

// listenersChanged reports whether the desired listener statuses differ from
// the existing ones. It checks listener count, attached routes per listener,
// and each listener's conditions.
func listenersChanged(existing []gatewayv1.ListenerStatus, desired []*acgatewayv1.ListenerStatusApplyConfiguration, generation int64) bool {
	if len(existing) != len(desired) {
		return true
	}
	for _, d := range desired {
		if d.Name == nil {
			return true
		}
		var e *gatewayv1.ListenerStatus
		for i := range existing {
			if existing[i].Name == gatewayv1.SectionName(*d.Name) {
				e = &existing[i]
				break
			}
		}
		if e == nil {
			return true
		}
		if d.AttachedRoutes != nil && e.AttachedRoutes != *d.AttachedRoutes {
			return true
		}
		for i := range d.Conditions {
			if applyConditionChanged(e.Conditions, &d.Conditions[i], generation) {
				return true
			}
		}
	}
	return false
}

// routeReferenceGrantsCondition builds a Gateway status condition reporting whether
// all cross-namespace HTTPRoutes have valid ReferenceGrants. When denied routes exist,
// the condition lists each one in a multi-line message.
func routeReferenceGrantsCondition(generation int64, now metav1.Time, deniedRoutes []*gatewayv1.HTTPRoute) *acmetav1.ConditionApplyConfiguration {
	if len(deniedRoutes) == 0 {
		return acmetav1.Condition().
			WithType(apiv1.ConditionRouteReferenceGrants).
			WithStatus(metav1.ConditionTrue).
			WithObservedGeneration(generation).
			WithLastTransitionTime(now).
			WithReason(apiv1.ReasonReferencesAllowed).
			WithMessage("All HTTPRoutes have valid references")
	}
	var lines []string
	for _, hr := range deniedRoutes {
		lines = append(lines, fmt.Sprintf("- %s/%s", hr.Namespace, hr.Name))
	}
	msg := fmt.Sprintf("HTTPRoutes denied due to missing or failed ReferenceGrant:\n%s", strings.Join(lines, "\n"))
	return acmetav1.Condition().
		WithType(apiv1.ConditionRouteReferenceGrants).
		WithStatus(metav1.ConditionFalse).
		WithObservedGeneration(generation).
		WithLastTransitionTime(now).
		WithReason(apiv1.ReasonReferencesDenied).
		WithMessage(msg)
}

// backendReferenceGrantsCondition builds a Gateway status condition reporting whether
// all cross-namespace backendRefs have valid ReferenceGrants. When denied refs exist,
// the condition lists each one in a multi-line message.
func backendReferenceGrantsCondition(generation int64, now metav1.Time, denied []deniedBackendRef) *acmetav1.ConditionApplyConfiguration {
	if len(denied) == 0 {
		return acmetav1.Condition().
			WithType(apiv1.ConditionBackendReferenceGrants).
			WithStatus(metav1.ConditionTrue).
			WithObservedGeneration(generation).
			WithLastTransitionTime(now).
			WithReason(apiv1.ReasonReferencesAllowed).
			WithMessage("All backendRefs have valid references")
	}
	var lines []string
	for _, d := range denied {
		lines = append(lines, fmt.Sprintf("- HTTPRoute %s/%s → Service %s/%s", d.routeNamespace, d.routeName, d.serviceNamespace, d.serviceName))
	}
	msg := fmt.Sprintf("BackendRefs denied due to missing or failed ReferenceGrant:\n%s", strings.Join(lines, "\n"))
	return acmetav1.Condition().
		WithType(apiv1.ConditionBackendReferenceGrants).
		WithStatus(metav1.ConditionFalse).
		WithObservedGeneration(generation).
		WithLastTransitionTime(now).
		WithReason(apiv1.ReasonReferencesDenied).
		WithMessage(msg)
}

// listGatewayRoutes lists all non-deleting HTTPRoutes that reference the given Gateway.
// Cross-namespace routes are only included if a ReferenceGrant in the Gateway's namespace
// permits the reference. Routes denied due to missing or failed ReferenceGrant checks are
// returned separately so the caller can report them without blocking reconciliation.
func listGatewayRoutes(ctx context.Context, r client.Reader, gw *gatewayv1.Gateway) (allowed, denied []*gatewayv1.HTTPRoute, err error) {
	var allRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &allRoutes); err != nil {
		return nil, nil, fmt.Errorf("listing HTTPRoutes: %w", err)
	}
	for i := range allRoutes.Items {
		hr := &allRoutes.Items[i]
		if !hr.DeletionTimestamp.IsZero() {
			continue
		}
		matches := false
		for _, ref := range hr.Spec.ParentRefs {
			if parentRefMatches(ref, gw, hr.Namespace) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		granted, grantErr := httpRouteReferenceGranted(ctx, r, hr.Namespace, gw)
		if grantErr != nil {
			return nil, nil, fmt.Errorf("checking ReferenceGrant for HTTPRoute %s/%s: %w", hr.Namespace, hr.Name, grantErr)
		}
		if !granted {
			denied = append(denied, hr)
			continue
		}
		allowed = append(allowed, hr)
	}
	slices.SortFunc(allowed, func(a, b *gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	slices.SortFunc(denied, func(a, b *gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return allowed, denied, nil
}

// deniedBackendRef holds information about a backendRef that was denied due to a
// missing or failed cross-namespace ReferenceGrant check.
type deniedBackendRef struct {
	routeNamespace   string
	routeName        string
	serviceNamespace string
	serviceName      string
}

// buildIngressRules converts a list of HTTPRoutes into Cloudflare tunnel ingress rules.
// For each rule in each route it takes the first backendRef and maps every hostname to
// http://<service>.<namespace>.svc.cluster.local:<port>, optionally with a path prefix.
// Only the first backendRef per rule is used because additional backendRefs represent
// traffic splitting (weighted load balancing), which Cloudflare tunnel ingress does not
// support — use a Kubernetes Service for that instead.
// Cross-namespace backendRefs are validated against ReferenceGrants. Denied refs are
// returned separately so the caller can report them without blocking reconciliation.
// A catch-all 404 rule is appended.
func buildIngressRules(ctx context.Context, r client.Reader, routes []*gatewayv1.HTTPRoute) ([]cfclient.IngressRule, []deniedBackendRef, error) {
	var rules []cfclient.IngressRule
	var denied []deniedBackendRef
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			if len(rule.BackendRefs) == 0 {
				continue
			}
			ref := rule.BackendRefs[0]
			ns := route.Namespace
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}
			granted, err := backendReferenceGranted(ctx, r, route.Namespace, ns, string(ref.Name))
			if err != nil {
				return nil, nil, fmt.Errorf("checking ReferenceGrant for backendRef %s/%s in HTTPRoute %s/%s: %w", ns, ref.Name, route.Namespace, route.Name, err)
			}
			if !granted {
				denied = append(denied, deniedBackendRef{
					routeNamespace:   route.Namespace,
					routeName:        route.Name,
					serviceNamespace: ns,
					serviceName:      string(ref.Name),
				})
				continue
			}
			port := int32(80)
			if ref.Port != nil {
				port = int32(*ref.Port)
			}
			service := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", string(ref.Name), ns, port)
			path := pathFromMatches(rule.Matches)
			for _, hostname := range route.Spec.Hostnames {
				rules = append(rules, cfclient.IngressRule{
					Hostname: string(hostname),
					Service:  service,
					Path:     path,
				})
			}
		}
	}
	// Append catch-all rule.
	rules = append(rules, cfclient.IngressRule{
		Service: "http_status:404",
	})
	return rules, denied, nil
}

// pathFromMatches extracts a path prefix from the first HTTPRouteMatch that has
// a PathPrefix match. Returns empty string if no path match is specified.
func pathFromMatches(matches []gatewayv1.HTTPRouteMatch) string {
	for _, m := range matches {
		if m.Path == nil {
			continue
		}
		if m.Path.Type == nil || *m.Path.Type == gatewayv1.PathMatchPathPrefix {
			if m.Path.Value != nil {
				return *m.Path.Value
			}
		}
	}
	return ""
}

// countAttachedRoutes counts the number of non-deleting HTTPRoutes attached to each
// listener of the given Gateway. Routes without a sectionName count toward all listeners.
func countAttachedRoutes(ctx context.Context, r client.Reader, gw *gatewayv1.Gateway) (map[gatewayv1.SectionName]int32, error) {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes); err != nil {
		return nil, fmt.Errorf("listing HTTPRoutes: %w", err)
	}
	counts := make(map[gatewayv1.SectionName]int32)
	for i := range routes.Items {
		hr := &routes.Items[i]
		if !hr.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ref := range hr.Spec.ParentRefs {
			if !parentRefMatches(ref, gw, hr.Namespace) {
				continue
			}
			if ref.SectionName != nil {
				counts[*ref.SectionName]++
			} else {
				// Attach to all listeners.
				for _, l := range gw.Spec.Listeners {
					counts[l.Name]++
				}
			}
		}
	}
	return counts, nil
}

func buildTunnelTokenSecret(gw *gatewayv1.Gateway, tunnelToken string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        apiv1.TunnelTokenSecretName(gw),
			Namespace:   gw.Namespace,
			Labels:      infrastructureLabels(gw.Spec.Infrastructure),
			Annotations: infrastructureAnnotations(gw.Spec.Infrastructure),
		},
		Data: map[string][]byte{
			"TUNNEL_TOKEN": []byte(tunnelToken),
		},
	}
}

func (r *GatewayReconciler) buildCloudflaredDeploymentApply(gw *gatewayv1.Gateway, replicas *int32) *acappsv1.DeploymentApplyConfiguration {
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": filepath.Base(apiv1.ControllerName),
		"app.kubernetes.io/instance":   gw.Name,
	}
	templateLabels := maps.Clone(selectorLabels)

	deployLabels := infrastructureLabels(gw.Spec.Infrastructure)
	deployAnnotations := infrastructureAnnotations(gw.Spec.Infrastructure)
	maps.Copy(templateLabels, deployLabels)
	templateAnnotations := infrastructureAnnotations(gw.Spec.Infrastructure)

	deploy := acappsv1.Deployment(apiv1.CloudflaredDeploymentName(gw), gw.Namespace).
		WithLabels(deployLabels).
		WithAnnotations(deployAnnotations).
		WithOwnerReferences(acmetav1.OwnerReference().
			WithAPIVersion(gatewayv1.GroupVersion.String()).
			WithKind(apiv1.KindGateway).
			WithName(gw.Name).
			WithUID(gw.UID).
			WithBlockOwnerDeletion(true).
			WithController(true),
		).
		WithSpec(acappsv1.DeploymentSpec().
			WithSelector(acmetav1.LabelSelector().
				WithMatchLabels(selectorLabels),
			).
			WithTemplate(accorev1.PodTemplateSpec().
				WithLabels(templateLabels).
				WithAnnotations(templateAnnotations).
				WithSpec(accorev1.PodSpec().
					WithContainers(accorev1.Container().
						WithName("cloudflared").
						WithImage(r.CloudflaredImage).
						WithArgs("tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run").
						WithEnv(accorev1.EnvVar().
							WithName("TUNNEL_TOKEN").
							WithValueFrom(accorev1.EnvVarSource().
								WithSecretKeyRef(accorev1.SecretKeySelector().
									WithName(apiv1.TunnelTokenSecretName(gw)).
									WithKey("TUNNEL_TOKEN"),
								),
							),
						).
						WithLivenessProbe(accorev1.Probe().
							WithHTTPGet(accorev1.HTTPGetAction().
								WithPath("/ready").
								WithPort(intstr.FromInt32(2000)),
							),
						),
					),
				),
			),
		)

	if replicas != nil {
		deploy.Spec.WithReplicas(*replicas)
	}

	return deploy
}

// infrastructureLabels returns the labels from the Gateway's infrastructure spec.
func infrastructureLabels(infra *gatewayv1.GatewayInfrastructure) map[string]string {
	if infra == nil {
		return nil
	}
	labels := make(map[string]string, len(infra.Labels))
	for k, v := range infra.Labels {
		labels[string(k)] = string(v)
	}
	return labels
}

// infrastructureAnnotations returns the annotations from the Gateway's infrastructure spec.
func infrastructureAnnotations(infra *gatewayv1.GatewayInfrastructure) map[string]string {
	if infra == nil {
		return nil
	}
	annotations := make(map[string]string, len(infra.Annotations))
	for k, v := range infra.Annotations {
		annotations[string(k)] = string(v)
	}
	return annotations
}
