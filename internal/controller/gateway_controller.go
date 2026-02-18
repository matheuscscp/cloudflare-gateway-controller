// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/fluxcd/pkg/ssa"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	"sigs.k8s.io/yaml"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

// DefaultCloudflaredImage is the default cloudflared container image.
// This value is updated automatically by the upgrade-cloudflared workflow.
const DefaultCloudflaredImage = "ghcr.io/matheuscscp/cloudflare-gateway-controller/cloudflared:2026.2.0@sha256:404528c1cd63c3eb882c257ae524919e4376115e6fe57befca8d603656a91a4c"

// ssaApplyOptions configures Server-Side Apply to recreate objects with
// immutable field changes and to clean up field managers left behind by
// kubectl so the controller can reclaim full ownership of managed fields.
var ssaApplyOptions = ssa.ApplyOptions{
	Force: true,
	Cleanup: ssa.ApplyCleanupOptions{
		Annotations: []string{
			corev1.LastAppliedConfigAnnotation,
		},
		FieldManagers: []ssa.FieldManager{
			{
				// Undo changes made with 'kubectl apply --server-side --force-conflicts'.
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationApply,
			},
			{
				// Undo changes made with 'kubectl create', 'kubectl scale', etc.
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
			{
				// Undo changes made with 'kubectl edit' or 'kubectl patch'.
				Name:          "kubectl-edit",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
			{
				// Undo changes made with 'kubectl apply' (client-side apply).
				Name:          "kubectl-client-side-apply",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
			{
				// Reclaim fields from objects created before the first SSA apply,
				// e.g. if someone pre-creates a Deployment matching our naming convention.
				Name:          "before-first-apply",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
		},
	},
}

// GatewayReconciler reconciles Gateway objects.
type GatewayReconciler struct {
	client.Client
	events.EventRecorder
	ResourceManager     *ssa.ResourceManager
	NewCloudflareClient cloudflare.ClientFactory
	CloudflaredImage    string
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=update;patch
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
		return ctrl.Result{}, err
	}
	if gc.Spec.ControllerName != apiv1.ControllerName {
		return ctrl.Result{}, nil
	}

	// Prune managed resources if the object is under deletion.
	if !gw.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &gw, &gc)
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

	// Ensure GatewayClass finalizer
	if err := r.ensureGatewayClassFinalizer(ctx, gc, gw); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("ensuring GatewayClass finalizer: %w", err))
	}

	// List all HTTPRoutes referencing this Gateway once, then reuse the list
	// for attached route counts, ingress rules, DNS, and status updates.
	var allRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &allRoutes, client.MatchingFields{
		indexHTTPRouteParentGateway: gw.Namespace + "/" + gw.Name,
	}); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("listing HTTPRoutes: %w", err))
	}

	desiredListeners := buildListenerStatuses(gw, countAttachedRoutes(&allRoutes, gw))
	now := metav1.Now()

	// Read credentials
	cfg, err := readCredentials(ctx, r.Client, gc, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("reading credentials: %w", err),
			metav1.Condition{
				Type:    string(gatewayv1.GatewayConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.GatewayReasonInvalidParameters),
				Message: fmt.Sprintf("Failed to read credentials: %v", err),
			})
	}

	// Create cloudflare client
	tc, err := r.NewCloudflareClient(cfg)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("creating cloudflare client: %w", err))
	}

	// Look up or create tunnel. The name is deterministic (gateway-{UID}),
	// so Cloudflare is the source of truth — no local state tracking needed.
	var changes []string
	name := apiv1.TunnelName(gw)
	tunnelID, err := tc.GetTunnelIDByName(ctx, name)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("looking up tunnel: %w", err))
	}
	if tunnelID == "" {
		tunnelID, err = tc.CreateTunnel(ctx, name)
		if err != nil {
			if !cloudflare.IsConflict(err) {
				return r.reconcileError(ctx, gw, fmt.Errorf("creating tunnel: %w", err))
			}
			tunnelID, err = tc.GetTunnelIDByName(ctx, name)
			if err != nil {
				return r.reconcileError(ctx, gw, fmt.Errorf("looking up tunnel after conflict: %w", err))
			}
		}
		changes = append(changes, "created tunnel")
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
	if secretResult != controllerutil.OperationResultNone {
		changes = append(changes, fmt.Sprintf("tunnel token Secret %s", secretResult))
	}
	log.V(1).Info("Reconciled tunnel token Secret", "result", secretResult)

	// Apply cloudflared Deployment via Server-Side Apply with dry-run diff.
	// Only the fields we declare are managed; the SSA manager detects drift
	// by comparing a server-side dry-run result with the existing object,
	// skipping the actual apply when no drift is detected.
	deployObj, err := r.buildCloudflaredDeployment(gw)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("building cloudflared deployment: %w", err))
	}
	deployEntry, err := r.ResourceManager.Apply(ctx, deployObj, ssaApplyOptions)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("applying cloudflared deployment: %w", err))
	}
	if string(deployEntry.Action) != string(ssa.UnchangedAction) {
		changes = append(changes, fmt.Sprintf("cloudflared Deployment %s", deployEntry.Action))
	}
	log.V(1).Info("Reconciled cloudflared Deployment", "action", deployEntry.Action)

	// Filter HTTPRoutes by parentRef and ReferenceGrant, and update tunnel ingress.
	gatewayRoutes, deniedRoutes, staleRoutes, err := listGatewayRoutes(ctx, r.Client, &allRoutes, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	// Remove stale status.parents entries for routes whose parentRef was removed.
	for _, route := range staleRoutes {
		if err := r.removeRouteStatus(ctx, gw, route); err != nil {
			log.Error(err, "Failed to remove stale status for HTTPRoute", "httproute", route.Namespace+"/"+route.Name)
		}
	}
	// Set ResolvedRefs=False/RefNotPermitted on denied routes (cross-namespace
	// parentRef not permitted by any ReferenceGrant).
	for _, route := range deniedRoutes {
		if err := r.updateDeniedRouteStatus(ctx, gw, route); err != nil {
			log.Error(err, "Failed to update denied status for HTTPRoute", "httproute", route.Namespace+"/"+route.Name)
		}
	}

	ingress, routesWithDeniedRefs, err := buildIngressRules(ctx, r.Client, gatewayRoutes)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	if len(routesWithDeniedRefs) > 0 {
		log.Info("BackendRefs denied due to missing or failed ReferenceGrant checks", "routes", len(routesWithDeniedRefs))
	}
	currentIngress, err := tc.GetTunnelConfiguration(ctx, tunnelID)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("getting tunnel configuration: %w", err))
	}
	if !ingressRulesEqual(currentIngress, ingress) {
		if err := tc.UpdateTunnelConfiguration(ctx, tunnelID, ingress); err != nil {
			return r.reconcileError(ctx, gw, fmt.Errorf("updating tunnel configuration: %w", err))
		}
		changes = append(changes, "updated tunnel configuration")
	}

	// Reconcile DNS CNAME records.
	zoneName := gw.Annotations[apiv1.AnnotationZoneName]
	dnsChanges, dnsErr := r.reconcileDNS(ctx, tc, tunnelID, zoneName, gw, gatewayRoutes)
	changes = append(changes, dnsChanges...)

	// Check Deployment readiness and progress.
	var deploy appsv1.Deployment
	deployAvailable := false
	deployDeadlineExceeded := false
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: gw.Namespace,
		Name:      apiv1.CloudflaredDeploymentName(gw),
	}, &deploy); err == nil {
		for _, c := range deploy.Status.Conditions {
			switch {
			case c.Type == appsv1.DeploymentAvailable && c.Status == "True":
				deployAvailable = true
			case c.Type == appsv1.DeploymentProgressing && c.Status == "False":
				deployDeadlineExceeded = true
			}
		}
	}

	readyStatus := metav1.ConditionUnknown
	readyReason := apiv1.ReasonProgressingWithRetry
	readyMsg := ""
	programmedStatus := metav1.ConditionFalse
	programmedReason := string(gatewayv1.GatewayReasonPending)
	programmedMsg := "Waiting for cloudflared deployment to become ready"
	if deployDeadlineExceeded {
		readyStatus = metav1.ConditionFalse
		readyReason = apiv1.ReasonReconciliationFailed
		readyMsg = "cloudflared deployment exceeded progress deadline"
	} else if deployAvailable {
		programmedStatus = metav1.ConditionTrue
		programmedReason = string(gatewayv1.GatewayReasonProgrammed)
		programmedMsg = "Gateway is programmed"
		readyStatus = metav1.ConditionTrue
		readyReason = apiv1.ReasonReconciliationSucceeded
		readyMsg = "Gateway is ready"
	} else {
		readyReason = apiv1.ReasonProgressing
		readyMsg = "Waiting for cloudflared deployment to become ready"
	}

	// Update HTTPRoute status.parents for allowed routes (after DNS and
	// Deployment checks so we can report DNS status and Gateway readiness).
	r.updateRouteStatuses(ctx, gw, gatewayRoutes, routesWithDeniedRefs, zoneName, dnsErr,
		readyStatus, readyReason, readyMsg)

	desiredConds := []metav1.Condition{
		{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonAccepted),
			Message:            "Gateway is accepted",
		},
		{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             programmedStatus,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             programmedReason,
			Message:            programmedMsg,
		},
		{
			Type:               apiv1.ConditionReady,
			Status:             readyStatus,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             readyReason,
			Message:            readyMsg,
		},
	}

	// Emit event for resource changes.
	if len(changes) > 0 {
		r.Eventf(gw, nil, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded, "Reconcile", strings.Join(changes, ", "))
	}

	// Skip the status patch if no conditions or listener statuses changed.
	requeueAfter := apiv1.ReconcileInterval(gw.Annotations)
	changed := false
	for _, c := range desiredConds {
		if conditions.Changed(gw.Status.Conditions, c.Type, c.Status, c.Reason, c.Message, gw.Generation) {
			changed = true
			break
		}
	}
	if !changed && listenersChanged(gw.Status.Listeners, desiredListeners) {
		changed = true
	}
	if !changed {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	patch := client.MergeFrom(gw.DeepCopy())
	gw.Status.Conditions = setConditions(gw.Status.Conditions, desiredConds, now)
	gw.Status.Listeners = desiredListeners
	if err := r.Status().Patch(ctx, gw, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileError handles business logic errors by best-effort patching Ready=Unknown
// on the Gateway status and emitting a Warning event. The status is always patched
// (even when the condition values haven't changed) so the resource version bumps,
// signalling that the controller is alive and actively retrying. Callers may pass
// additional conditions (e.g. Accepted=False for credential errors). Other existing
// conditions are preserved.
func (r *GatewayReconciler) reconcileError(ctx context.Context, gw *gatewayv1.Gateway, reconcileErr error, extraConds ...metav1.Condition) (ctrl.Result, error) {
	msg := reconcileErr.Error()
	now := metav1.Now()
	desiredConds := append([]metav1.Condition{
		{
			Type:               apiv1.ConditionReady,
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             apiv1.ReasonProgressingWithRetry,
			Message:            msg,
		},
	}, extraConds...)
	for i := range desiredConds[1:] {
		desiredConds[i+1].ObservedGeneration = gw.Generation
		desiredConds[i+1].LastTransitionTime = now
	}

	patch := client.MergeFrom(gw.DeepCopy())
	for _, c := range desiredConds {
		gw.Status.Conditions = setCondition(gw.Status.Conditions, c)
	}
	if err := r.Status().Patch(ctx, gw, patch); err != nil {
		log.FromContext(ctx).Error(err, "faield to patch status with error",
			"originalError", msg)
	}

	r.Eventf(gw, nil, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry, "Reconcile", "Reconciliation failed: %v", reconcileErr)
	return ctrl.Result{}, reconcileErr
}

// finalize cleans up managed resources (cloudflared Deployment and Cloudflare tunnel)
// before allowing the Gateway to be deleted, then removes the finalizer.
func (r *GatewayReconciler) finalize(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	// When reconciliation is disabled, remove owner references from managed
	// resources so Kubernetes GC doesn't delete them when the Gateway is removed.
	// The user is responsible for manually cleaning up.
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		removed, err := r.removeOwnerReferences(ctx, gw)
		defer func() {
			var removedInfo []string
			for _, obj := range removed {
				removedInfo = append(removedInfo, fmt.Sprintf("%s/%s/%s",
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetNamespace(),
					obj.GetName()))
			}
			if len(removedInfo) > 0 {
				log.FromContext(ctx).Info(
					"finalization: Gateway disabled, removed owner references from managed objects",
					"objects", removedInfo)
			}
			for _, obj := range removed {
				r.Eventf(obj, gw, corev1.EventTypeNormal, apiv1.ReasonReconciliationDisabled,
					"Finalize", "Gateway removed owner reference due to disabled finalization")
			}
		}()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("removing owner references: %w", err)
		}
	} else {
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
		tc, err := r.NewCloudflareClient(cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("creating cloudflare client for deletion: %w", err)
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

	// Remove this Gateway's entry from status.parents on all referencing HTTPRoutes.
	if err := r.removeRouteStatuses(ctx, gw); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing HTTPRoute status entries: %w", err)
	}

	// Remove this Gateway's finalizer from all GatewayClasses.
	if err := r.removeGatewayClassFinalizer(ctx, gw); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing GatewayClass finalizer: %w", err)
	}

	// Remove finalizer from Gateway if needed
	if controllerutil.ContainsFinalizer(gw, apiv1.Finalizer) {
		gwPatch := client.MergeFrom(gw.DeepCopy())
		controllerutil.RemoveFinalizer(gw, apiv1.Finalizer)
		if err := r.Patch(ctx, gw, gwPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// removeOwnerReferences removes the Gateway's owner references from managed
// resources (cloudflared Deployment and tunnel token Secret) so they survive
// garbage collection when the Gateway is deleted with reconciliation disabled.
// Returns the list of resources that were modified.
func (r *GatewayReconciler) removeOwnerReferences(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	var removed []client.Object

	// Remove owner reference from cloudflared Deployment.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: gw.Namespace, Name: apiv1.CloudflaredDeploymentName(gw)}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, deployKey, &deploy); err != nil {
			return client.IgnoreNotFound(err)
		}
		deployPatch := client.MergeFromWithOptions(deploy.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if !removeOwnerRef(&deploy, gw.UID) {
			return nil
		}
		if err := r.Patch(ctx, &deploy, deployPatch); err != nil {
			return fmt.Errorf("patching deployment: %w", err)
		}
		deploy.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(apiv1.KindDeployment))
		removed = append(removed, &deploy)
		return nil
	}); err != nil {
		return removed, err
	}

	// Remove owner reference from tunnel token Secret.
	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: gw.Namespace, Name: apiv1.TunnelTokenSecretName(gw)}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			return client.IgnoreNotFound(err)
		}
		secretPatch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if !removeOwnerRef(&secret, gw.UID) {
			return nil
		}
		if err := r.Patch(ctx, &secret, secretPatch); err != nil {
			return fmt.Errorf("patching secret: %w", err)
		}
		secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(apiv1.KindSecret))
		removed = append(removed, &secret)
		return nil
	}); err != nil {
		return removed, err
	}

	return removed, nil
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

// reconcileDNS reconciles DNS CNAME records for the Gateway's zone. When
// zoneName is empty and DNS was previously enabled (condition was True on any
// HTTPRoute), all CNAME records pointing to the tunnel across all account
// zones are deleted. Returns a non-nil error string on failure, which is
// reported on each HTTPRoute's DNSRecordsApplied condition.
func (r *GatewayReconciler) reconcileDNS(ctx context.Context, tc cloudflare.Client, tunnelID, zoneName string, gw *gatewayv1.Gateway, routes []*gatewayv1.HTTPRoute) ([]string, *string) {
	log := log.FromContext(ctx)

	if zoneName == "" {
		// Only clean up if DNS was previously enabled (condition was True
		// on any HTTPRoute, meaning records may exist). Skip API calls otherwise.
		if dnsWasPreviouslyEnabled(routes, gw) {
			if err := r.cleanupAllDNS(ctx, tc, tunnelID); err != nil {
				return nil, new(fmt.Sprintf("Failed to clean up DNS records: %v", err))
			}
			return []string{"cleaned up all DNS CNAME records"}, nil
		}
		return nil, nil
	}

	zoneID, err := tc.FindZoneIDByHostname(ctx, zoneName)
	if err != nil {
		return nil, new(fmt.Sprintf("Failed to find zone ID for %q: %v", zoneName, err))
	}

	// Compute desired hostnames from all attached HTTPRoutes.
	desired := make(map[string]struct{})
	for _, route := range routes {
		for _, h := range route.Spec.Hostnames {
			hostname := string(h)
			if hostnameInZone(hostname, zoneName) {
				desired[hostname] = struct{}{}
			}
		}
	}

	// Query actual CNAME records pointing to our tunnel.
	tunnelTarget := cloudflare.TunnelTarget(tunnelID)
	actual, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
	if err != nil {
		return nil, new(fmt.Sprintf("Failed to list DNS CNAMEs: %v", err))
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, h := range actual {
		actualSet[h] = struct{}{}
	}

	// Create missing records.
	var dnsChanges []string
	for h := range desired {
		if _, ok := actualSet[h]; !ok {
			if err := tc.EnsureDNSCNAME(ctx, zoneID, h, tunnelTarget); err != nil {
				return dnsChanges, new(fmt.Sprintf("Failed to ensure DNS CNAME for %q: %v", h, err))
			}
			dnsChanges = append(dnsChanges, fmt.Sprintf("created DNS CNAME for %s", h))
			log.V(1).Info("Created DNS CNAME", "hostname", h)
		}
	}

	// Delete stale records.
	for h := range actualSet {
		if _, ok := desired[h]; !ok {
			if err := tc.DeleteDNSCNAME(ctx, zoneID, h); err != nil {
				return dnsChanges, new(fmt.Sprintf("Failed to delete stale DNS CNAME for %q: %v", h, err))
			}
			dnsChanges = append(dnsChanges, fmt.Sprintf("deleted DNS CNAME for %s", h))
			log.V(1).Info("Deleted stale DNS CNAME", "hostname", h)
		}
	}

	return dnsChanges, nil
}

// dnsWasPreviouslyEnabled checks whether any HTTPRoute's status.parents entry
// for the given Gateway has a DNSRecordsApplied condition with True status.
func dnsWasPreviouslyEnabled(routes []*gatewayv1.HTTPRoute, gw *gatewayv1.Gateway) bool {
	for _, route := range routes {
		existing := findRouteParentStatus(route.Status.Parents, gw)
		if existing == nil {
			continue
		}
		if conditions.Find(existing.Conditions, apiv1.ConditionDNSRecordsApplied) != nil {
			return true
		}
	}
	return false
}

// cleanupAllDNS deletes all CNAME records pointing to the tunnel across all
// account zones. This is used when the zoneName annotation is removed.
func (r *GatewayReconciler) cleanupAllDNS(ctx context.Context, tc cloudflare.Client, tunnelID string) error {
	log := log.FromContext(ctx)
	tunnelTarget := cloudflare.TunnelTarget(tunnelID)

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
// If the Gateway's gatewayClassName was changed, the stale finalizer on the
// previous GatewayClass is removed first.
func (r *GatewayReconciler) ensureGatewayClassFinalizer(ctx context.Context, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway) error {
	finalizer := apiv1.FinalizerGatewayClass(gw)

	// Remove the stale finalizer from any other GatewayClass (handles
	// gatewayClassName changes).
	if err := r.removeGatewayClassFinalizer(ctx, gw, gc.Name); err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Name: gc.Name}, gc); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(gc, finalizer) {
			return nil
		}
		gcPatch := client.MergeFromWithOptions(gc.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(gc, finalizer)
		return r.Patch(ctx, gc, gcPatch)
	})
}

// removeGatewayClassFinalizer removes the Gateway's finalizer from all
// GatewayClasses that carry it, optionally skipping one by name. This handles
// the case where the Gateway's gatewayClassName was changed, leaving a stale
// finalizer on the previous GatewayClass. During finalization skipName is empty
// so the finalizer is removed from all GatewayClasses; during reconciliation
// the current GatewayClass is skipped.
func (r *GatewayReconciler) removeGatewayClassFinalizer(ctx context.Context, gw *gatewayv1.Gateway, skipName ...string) error {
	finalizer := apiv1.FinalizerGatewayClass(gw)
	skip := ""
	if len(skipName) > 0 {
		skip = skipName[0]
	}

	var classes gatewayv1.GatewayClassList
	if err := r.List(ctx, &classes, client.MatchingFields{
		indexGatewayClassGatewayFinalizer: finalizer,
	}); err != nil {
		return fmt.Errorf("listing GatewayClasses with finalizer: %w", err)
	}
	for i := range classes.Items {
		gc := &classes.Items[i]
		if gc.Name == skip {
			continue
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, types.NamespacedName{Name: gc.Name}, gc); err != nil {
				return client.IgnoreNotFound(err)
			}
			if !controllerutil.ContainsFinalizer(gc, finalizer) {
				return nil
			}
			gcPatch := client.MergeFromWithOptions(gc.DeepCopy(), client.MergeFromWithOptimisticLock{})
			controllerutil.RemoveFinalizer(gc, finalizer)
			return r.Patch(ctx, gc, gcPatch)
		}); err != nil {
			return fmt.Errorf("removing finalizer from GatewayClass %s: %w", gc.Name, err)
		}
	}
	return nil
}

// readCredentials reads the Cloudflare API credentials from the Secret referenced
// by the Gateway's infrastructure parametersRef or the GatewayClass parametersRef.
// Cross-namespace references are validated against ReferenceGrants.
func readCredentials(ctx context.Context, r client.Reader, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway) (cloudflare.ClientConfig, error) {
	var secretNamespace, secretName string

	if gw.Spec.Infrastructure != nil && gw.Spec.Infrastructure.ParametersRef != nil {
		ref := gw.Spec.Infrastructure.ParametersRef
		if string(ref.Kind) != apiv1.KindSecret || (ref.Group != "" && ref.Group != "core" && ref.Group != gatewayv1.Group("")) {
			return cloudflare.ClientConfig{}, fmt.Errorf("infrastructure parametersRef must reference a core/v1 Secret")
		}
		secretNamespace = gw.Namespace
		secretName = string(ref.Name)
	} else {
		if gc.Spec.ParametersRef == nil {
			return cloudflare.ClientConfig{}, fmt.Errorf("gatewayclass %q has no parametersRef", gc.Name)
		}
		ref := gc.Spec.ParametersRef
		if string(ref.Kind) != apiv1.KindSecret || (ref.Group != "" && ref.Group != "core" && ref.Group != gatewayv1.Group("")) {
			return cloudflare.ClientConfig{}, fmt.Errorf("parametersRef must reference a core/v1 Secret")
		}
		if ref.Namespace == nil {
			return cloudflare.ClientConfig{}, fmt.Errorf("parametersRef must specify a namespace")
		}
		secretNamespace = string(*ref.Namespace)
		secretName = ref.Name

		if granted, err := secretReferenceGranted(ctx, r, gw.Namespace, secretNamespace, secretName); err != nil {
			return cloudflare.ClientConfig{}, fmt.Errorf("checking ReferenceGrant: %w", err)
		} else if !granted {
			return cloudflare.ClientConfig{}, fmt.Errorf("cross-namespace reference to Secret %s/%s not allowed by any ReferenceGrant", secretNamespace, secretName)
		}
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: secretNamespace,
		Name:      secretName,
	}, &secret); err != nil {
		return cloudflare.ClientConfig{}, fmt.Errorf("getting secret %s/%s: %w", secretNamespace, secretName, err)
	}

	apiToken := string(secret.Data["CLOUDFLARE_API_TOKEN"])
	accountID := string(secret.Data["CLOUDFLARE_ACCOUNT_ID"])
	if apiToken == "" || accountID == "" {
		return cloudflare.ClientConfig{}, fmt.Errorf("secret %s/%s must contain CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID", secretNamespace, secretName)
	}

	return cloudflare.ClientConfig{
		APIToken:  apiToken,
		AccountID: accountID,
	}, nil
}

// buildListenerStatuses builds the desired listener statuses for each Gateway listener,
// setting Accepted/Programmed conditions based on protocol support and the attached route count.
func buildListenerStatuses(gw *gatewayv1.Gateway, attachedRoutes map[gatewayv1.SectionName]int32) []gatewayv1.ListenerStatus {
	now := metav1.Now()
	statuses := make([]gatewayv1.ListenerStatus, 0, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		supported := l.Protocol == gatewayv1.HTTPProtocolType || l.Protocol == gatewayv1.HTTPSProtocolType

		var acceptedCond, programmedCond metav1.Condition
		if supported {
			acceptedCond = metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gw.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonAccepted),
				Message:            "Listener is accepted",
			}
			programmedCond = metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionProgrammed),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gw.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonProgrammed),
				Message:            "Listener is programmed",
			}
		} else {
			acceptedCond = metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonUnsupportedProtocol),
				Message:            fmt.Sprintf("Protocol %q is not supported", l.Protocol),
			}
			programmedCond = metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionProgrammed),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalid),
				Message:            "Listener is not programmed due to unsupported protocol",
			}
		}

		statuses = append(statuses, gatewayv1.ListenerStatus{
			Name: l.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: new(gatewayv1.Group(gatewayv1.GroupVersion.Group)),
					Kind:  apiv1.KindHTTPRoute,
				},
			},
			AttachedRoutes: attachedRoutes[l.Name],
			Conditions: []metav1.Condition{
				acceptedCond,
				programmedCond,
				{
					Type:               string(gatewayv1.ListenerConditionConflicted),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gw.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonNoConflicts),
					Message:            "No conflicts",
				},
				{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gw.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "References resolved",
				},
			},
		})
	}
	return statuses
}

// listenersChanged reports whether the desired listener statuses differ from
// the existing ones. It checks listener count, attached routes per listener,
// and each listener's conditions.
func listenersChanged(existing, desired []gatewayv1.ListenerStatus) bool {
	if len(existing) != len(desired) {
		return true
	}
	for _, d := range desired {
		var e *gatewayv1.ListenerStatus
		for i := range existing {
			if existing[i].Name == d.Name {
				e = &existing[i]
				break
			}
		}
		if e == nil {
			return true
		}
		if e.AttachedRoutes != d.AttachedRoutes {
			return true
		}
		for _, dc := range d.Conditions {
			if conditions.Changed(e.Conditions, dc.Type, dc.Status, dc.Reason, dc.Message, dc.ObservedGeneration) {
				return true
			}
		}
	}
	return false
}

// setConditions replaces the existing condition list with the desired ones, preserving
// LastTransitionTime when the status hasn't changed.
func setConditions(existing, desired []metav1.Condition, now metav1.Time) []metav1.Condition {
	result := make([]metav1.Condition, 0, len(desired))
	for _, d := range desired {
		c := d
		if prev := conditions.Find(existing, d.Type); prev != nil && prev.Status == d.Status {
			c.LastTransitionTime = prev.LastTransitionTime
		}
		result = append(result, c)
	}
	return result
}

// setCondition upserts a single condition in the list, preserving
// LastTransitionTime when the status hasn't changed and keeping other
// conditions untouched.
func setCondition(existing []metav1.Condition, desired metav1.Condition) []metav1.Condition {
	if prev := conditions.Find(existing, desired.Type); prev != nil {
		if prev.Status == desired.Status {
			desired.LastTransitionTime = prev.LastTransitionTime
		}
		*prev = desired
		return existing
	}
	return append(existing, desired)
}

// listGatewayRoutes filters non-deleting HTTPRoutes from the pre-fetched list that
// reference the given Gateway via spec.parentRefs. Cross-namespace routes are only
// included if a ReferenceGrant in the Gateway's namespace permits the reference.
// Routes denied due to missing or failed ReferenceGrant checks are returned
// separately so the caller can report them without blocking reconciliation.
// Routes that appear in allRoutes but do not have a matching parentRef (e.g. stale
// status.parents entries from a previous parentRef) are returned as stale.
func listGatewayRoutes(ctx context.Context, r client.Client, allRoutes *gatewayv1.HTTPRouteList, gw *gatewayv1.Gateway) (allowed, denied, stale []*gatewayv1.HTTPRoute, err error) {
	for i := range allRoutes.Items {
		hr := &allRoutes.Items[i]

		if !hr.DeletionTimestamp.IsZero() {
			continue
		}

		// Check if the route actually has a parentRef to this Gateway.
		hasParentRef := false
		for _, ref := range hr.Spec.ParentRefs {
			if parentRefMatches(ref, gw, hr.Namespace) {
				hasParentRef = true
				break
			}
		}
		if !hasParentRef {
			// Route appeared in the index via a stale status.parents entry.
			stale = append(stale, hr)
			continue
		}

		granted, grantErr := httpRouteReferenceGranted(ctx, r, hr.Namespace, gw)
		if grantErr != nil {
			return nil, nil, nil, fmt.Errorf("checking ReferenceGrant for HTTPRoute %s/%s: %w", hr.Namespace, hr.Name, grantErr)
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
	return allowed, denied, stale, nil
}

// buildIngressRules converts a list of HTTPRoutes into Cloudflare tunnel ingress rules.
// For each rule in each route it takes the first backendRef and maps every hostname to
// http://<service>.<namespace>.svc.cluster.local:<port>, optionally with a path prefix.
// Only the first backendRef per rule is used because additional backendRefs represent
// traffic splitting (weighted load balancing), which Cloudflare tunnel ingress does not
// support — use a Kubernetes Service for that instead.
// Cross-namespace backendRefs are validated against ReferenceGrants. Routes with denied
// refs are returned in a map (route name -> list of denied ref names) so the caller can
// set ResolvedRefs=False on those routes with a specific message.
// A catch-all 404 rule is appended.
func buildIngressRules(ctx context.Context, r client.Reader, routes []*gatewayv1.HTTPRoute) ([]cloudflare.IngressRule, map[types.NamespacedName][]string, error) {
	var rules []cloudflare.IngressRule
	routesWithDeniedRefs := make(map[types.NamespacedName][]string)
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
				key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
				routesWithDeniedRefs[key] = append(routesWithDeniedRefs[key], ns+"/"+string(ref.Name))
				continue
			}
			port := int32(80)
			if ref.Port != nil {
				port = int32(*ref.Port)
			}
			service := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", string(ref.Name), ns, port)
			path := pathFromMatches(rule.Matches)
			for _, hostname := range route.Spec.Hostnames {
				rules = append(rules, cloudflare.IngressRule{
					Hostname: string(hostname),
					Service:  service,
					Path:     path,
				})
			}
		}
	}
	// Append catch-all rule.
	rules = append(rules, cloudflare.IngressRule{
		Service: "http_status:404",
	})
	return rules, routesWithDeniedRefs, nil
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
func countAttachedRoutes(routes *gatewayv1.HTTPRouteList, gw *gatewayv1.Gateway) map[gatewayv1.SectionName]int32 {
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
	return counts
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

// buildCloudflaredDeployment builds the desired cloudflared Deployment as an
// unstructured object suitable for server-side apply via the SSA manager.
// If the Gateway has an AnnotationDeploymentPatches annotation, the RFC 6902
// JSON Patch operations (written in YAML) are applied to the base Deployment.
func (r *GatewayReconciler) buildCloudflaredDeployment(gw *gatewayv1.Gateway) (*unstructured.Unstructured, error) {
	apply := r.buildCloudflaredDeploymentApply(gw)
	data, err := json.Marshal(apply)
	if err != nil {
		return nil, fmt.Errorf("marshaling deployment: %w", err)
	}

	// Apply RFC 6902 JSON Patch operations from the annotation if present.
	if patchYAML, ok := gw.Annotations[apiv1.AnnotationDeploymentPatches]; ok {
		patchJSON, err := yaml.YAMLToJSON([]byte(patchYAML))
		if err != nil {
			return nil, fmt.Errorf("parsing deploymentPatches annotation YAML: %w", err)
		}
		patch, err := jsonpatch.DecodePatch(patchJSON)
		if err != nil {
			return nil, fmt.Errorf("decoding deploymentPatches annotation: %w", err)
		}
		data, err = patch.Apply(data)
		if err != nil {
			return nil, fmt.Errorf("applying deploymentPatches annotation: %w", err)
		}
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("unmarshaling deployment: %w", err)
	}
	obj.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(apiv1.KindDeployment))
	return obj, nil
}

func (r *GatewayReconciler) buildCloudflaredDeploymentApply(gw *gatewayv1.Gateway) *acappsv1.DeploymentApplyConfiguration {
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
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

// removeRouteStatuses removes this Gateway's entry from status.parents on all
// HTTPRoutes that reference it. This is called during Gateway finalization.
func (r *GatewayReconciler) removeRouteStatuses(ctx context.Context, gw *gatewayv1.Gateway) error {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{
		indexHTTPRouteParentGateway: gw.Namespace + "/" + gw.Name,
	}); err != nil {
		return fmt.Errorf("listing HTTPRoutes for gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	for i := range routes.Items {
		if err := r.removeRouteStatus(ctx, gw, &routes.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// removeRouteStatus removes this Gateway's entry from status.parents on a
// single HTTPRoute. This is used for stale routes (parentRef removed) and
// during Gateway finalization.
func (r *GatewayReconciler) removeRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute) error {
	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			return client.IgnoreNotFound(err)
		}
		if findRouteParentStatus(route.Status.Parents, gw) == nil {
			return nil
		}
		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		var filtered []gatewayv1.RouteParentStatus
		for _, s := range route.Status.Parents {
			if s.ControllerName == apiv1.ControllerName &&
				string(s.ParentRef.Name) == gw.Name &&
				s.ParentRef.Namespace != nil && string(*s.ParentRef.Namespace) == gw.Namespace {
				continue
			}
			filtered = append(filtered, s)
		}
		route.Status.Parents = filtered
		patched = true
		if err := r.Status().Patch(ctx, route, patch); err != nil {
			return fmt.Errorf("removing status entry from HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
		}
		return nil
	})
	if patched {
		r.Eventf(route, gw, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded,
			"Reconcile", "Removed status entry for Gateway %s/%s", gw.Namespace, gw.Name)
	}
	return err
}

// updateDeniedRouteStatus sets ResolvedRefs=False/RefNotPermitted on the
// status.parents entry for this Gateway on a denied HTTPRoute (cross-namespace
// parentRef not permitted by any ReferenceGrant). Any stale conditions from a
// previous reconciliation (e.g. Accepted, DNS) are removed.
func (r *GatewayReconciler) updateDeniedRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute) error {
	resolvedRefsType := string(gatewayv1.RouteConditionResolvedRefs)
	resolvedRefsStatus := metav1.ConditionFalse
	resolvedRefsReason := string(gatewayv1.RouteReasonRefNotPermitted)
	resolvedRefsMsg := fmt.Sprintf("Cross-namespace parentRef to Gateway %s/%s not permitted by any ReferenceGrant", gw.Namespace, gw.Name)

	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			return client.IgnoreNotFound(err)
		}

		existing := findRouteParentStatus(route.Status.Parents, gw)

		// Skip if the entry already has exactly the right condition.
		if existing != nil &&
			len(existing.Conditions) == 1 &&
			!conditions.Changed(existing.Conditions, resolvedRefsType, resolvedRefsStatus,
				resolvedRefsReason, resolvedRefsMsg, route.Generation) {
			return nil
		}

		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()

		if existing == nil {
			route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{
					Group:     new(gatewayv1.Group(gatewayv1.GroupName)),
					Kind:      new(gatewayv1.Kind(apiv1.KindGateway)),
					Namespace: new(gatewayv1.Namespace(gw.Namespace)),
					Name:      gatewayv1.ObjectName(gw.Name),
				},
				ControllerName: apiv1.ControllerName,
			})
			existing = &route.Status.Parents[len(route.Status.Parents)-1]
		}

		// Replace all conditions with just ResolvedRefs=False/RefNotPermitted,
		// removing any stale Accepted or DNS conditions from a previous
		// reconciliation when the route was allowed.
		existing.Conditions = setConditions(existing.Conditions, []metav1.Condition{
			{
				Type:               resolvedRefsType,
				Status:             resolvedRefsStatus,
				ObservedGeneration: route.Generation,
				LastTransitionTime: now,
				Reason:             resolvedRefsReason,
				Message:            resolvedRefsMsg,
			},
		}, now)

		patched = true
		return r.Status().Patch(ctx, route, patch)
	})
	if patched {
		r.Eventf(route, gw, corev1.EventTypeWarning, resolvedRefsReason,
			"Reconcile", resolvedRefsMsg)
	}
	return err
}

// updateRouteStatuses updates the status.parents entry for this Gateway
// on each allowed HTTPRoute using merge-patch.
func (r *GatewayReconciler) updateRouteStatuses(ctx context.Context, gw *gatewayv1.Gateway, routes []*gatewayv1.HTTPRoute, routesWithDeniedRefs map[types.NamespacedName][]string, zoneName string, dnsErr *string, readyStatus metav1.ConditionStatus, readyReason, readyMsg string) {
	log := log.FromContext(ctx)
	for _, route := range routes {
		deniedRefs := routesWithDeniedRefs[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
		if err := r.updateRouteStatus(ctx, gw, route, deniedRefs, zoneName, dnsErr, readyStatus, readyReason, readyMsg); err != nil {
			log.Error(err, "Failed to update HTTPRoute status", "httproute", route.Namespace+"/"+route.Name)
		}
	}
}

// updateRouteStatus updates the status.parents entry for this Gateway on the
// given HTTPRoute using merge-patch. If the entry already exists with up-to-date
// conditions, no patch is issued.
func (r *GatewayReconciler) updateRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute, deniedRefs []string, zoneName string, dnsErr *string, readyStatus metav1.ConditionStatus, readyReason, readyMsg string) error {
	acceptedType := string(gatewayv1.RouteConditionAccepted)
	resolvedRefsType := string(gatewayv1.RouteConditionResolvedRefs)

	// ResolvedRefs condition: False/RefNotPermitted when cross-namespace backendRefs
	// are denied, True/ResolvedRefs otherwise.
	resolvedRefsStatus := metav1.ConditionTrue
	resolvedRefsReason := string(gatewayv1.RouteReasonResolvedRefs)
	resolvedRefsMsg := "References resolved"
	if len(deniedRefs) > 0 {
		resolvedRefsStatus = metav1.ConditionFalse
		resolvedRefsReason = string(gatewayv1.RouteReasonRefNotPermitted)
		resolvedRefsMsg = "BackendRefs not permitted by ReferenceGrant:\n"
		for _, ref := range deniedRefs {
			resolvedRefsMsg += "- " + ref + "\n"
		}
		resolvedRefsMsg = strings.TrimSuffix(resolvedRefsMsg, "\n")
	}

	// Build per-route DNS condition if DNS is enabled on the Gateway.
	dnsEnabled := zoneName != ""
	var dnsStatus metav1.ConditionStatus
	var dnsReason, dnsMessage string
	if dnsEnabled {
		if dnsErr != nil {
			dnsStatus = metav1.ConditionFalse
			dnsReason = apiv1.ReasonReconciliationFailed
			dnsMessage = *dnsErr
		} else {
			dnsStatus = metav1.ConditionTrue
			dnsReason = apiv1.ReasonReconciliationSucceeded
			dnsMessage = routeDNSMessage(route, zoneName)
		}
	}

	// Compute route-level Ready condition. Start with the Gateway's Ready state,
	// then downgrade to Unknown/ProgressingWithRetry if there's a DNS error.
	routeReadyStatus := readyStatus
	routeReadyReason := readyReason
	routeReadyMsg := readyMsg
	if readyStatus == metav1.ConditionTrue && dnsErr != nil {
		routeReadyStatus = metav1.ConditionUnknown
		routeReadyReason = apiv1.ReasonProgressingWithRetry
		routeReadyMsg = *dnsErr
	}

	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			return err
		}

		existing := findRouteParentStatus(route.Status.Parents, gw)

		// Check if update is needed.
		if existing != nil {
			changed := conditions.Changed(existing.Conditions, acceptedType, metav1.ConditionTrue,
				string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", route.Generation) ||
				conditions.Changed(existing.Conditions, resolvedRefsType, resolvedRefsStatus,
					resolvedRefsReason, resolvedRefsMsg, route.Generation)
			if dnsEnabled {
				changed = changed || conditions.Changed(existing.Conditions, apiv1.ConditionDNSRecordsApplied,
					dnsStatus, dnsReason, dnsMessage, route.Generation)
			} else {
				// DNS was disabled — check if we need to remove a stale condition.
				changed = changed || conditions.Find(existing.Conditions, apiv1.ConditionDNSRecordsApplied) != nil
			}
			changed = changed || conditions.Changed(existing.Conditions, apiv1.ConditionReady,
				routeReadyStatus, routeReadyReason, routeReadyMsg, route.Generation)
			if !changed {
				return nil
			}
		}

		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()

		if existing == nil {
			route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{
					Group:     new(gatewayv1.Group(gatewayv1.GroupName)),
					Kind:      new(gatewayv1.Kind(apiv1.KindGateway)),
					Namespace: new(gatewayv1.Namespace(gw.Namespace)),
					Name:      gatewayv1.ObjectName(gw.Name),
				},
				ControllerName: apiv1.ControllerName,
			})
			existing = &route.Status.Parents[len(route.Status.Parents)-1]
		}

		setRouteCondition(existing, acceptedType, metav1.ConditionTrue,
			string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", route.Generation, now)
		setRouteCondition(existing, resolvedRefsType, resolvedRefsStatus,
			resolvedRefsReason, resolvedRefsMsg, route.Generation, now)

		if dnsEnabled {
			setRouteCondition(existing, apiv1.ConditionDNSRecordsApplied, dnsStatus,
				dnsReason, dnsMessage, route.Generation, now)
		} else {
			removeRouteCondition(existing, apiv1.ConditionDNSRecordsApplied)
		}

		setRouteCondition(existing, apiv1.ConditionReady, routeReadyStatus,
			routeReadyReason, routeReadyMsg, route.Generation, now)

		patched = true
		return r.Status().Patch(ctx, route, patch)
	})
	if patched {
		eventType := corev1.EventTypeNormal
		if routeReadyStatus != metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Eventf(route, gw, eventType, routeReadyReason,
			"Reconcile", "Ready=%s: %s", routeReadyStatus, routeReadyMsg)
	}
	return err
}

// routeDNSMessage builds a per-route DNS condition message listing the
// hostnames that were applied and those that were skipped (not in zone).
func routeDNSMessage(route *gatewayv1.HTTPRoute, zoneName string) string {
	var applied, skipped []string
	for _, h := range route.Spec.Hostnames {
		hostname := string(h)
		if hostnameInZone(hostname, zoneName) {
			applied = append(applied, hostname)
		} else {
			skipped = append(skipped, hostname)
		}
	}
	var msg strings.Builder
	msg.WriteString("Applied hostnames:")
	if len(applied) == 0 {
		msg.WriteString("\n(none)")
	} else {
		for _, h := range applied {
			fmt.Fprintf(&msg, "\n- %s", h)
		}
	}
	msg.WriteString("\nSkipped hostnames (not in zone):")
	if len(skipped) == 0 {
		msg.WriteString("\n(none)")
	} else {
		for _, h := range skipped {
			fmt.Fprintf(&msg, "\n- %s", h)
		}
	}
	return msg.String()
}

// ingressRulesEqual reports whether two slices of ingress rules contain the
// same rules regardless of order. It sorts copies of both slices before comparing.
func ingressRulesEqual(a, b []cloudflare.IngressRule) bool {
	cmp := func(x, y cloudflare.IngressRule) int {
		if c := strings.Compare(x.Hostname, y.Hostname); c != 0 {
			return c
		}
		if c := strings.Compare(x.Service, y.Service); c != 0 {
			return c
		}
		return strings.Compare(x.Path, y.Path)
	}
	sortedA := slices.Clone(a)
	sortedB := slices.Clone(b)
	slices.SortFunc(sortedA, cmp)
	slices.SortFunc(sortedB, cmp)
	return slices.Equal(sortedA, sortedB)
}

// hostnameInZone reports whether a hostname is a direct (single-level)
// subdomain of zoneName. For example, "app.example.com" matches zone
// "example.com", but "deep.sub.example.com" and "example.com" do not.
func hostnameInZone(hostname, zoneName string) bool {
	prefix, ok := strings.CutSuffix(hostname, "."+zoneName)
	return ok && !strings.Contains(prefix, ".")
}

// removeRouteCondition removes a condition by type from the RouteParentStatus.
func removeRouteCondition(parent *gatewayv1.RouteParentStatus, condType string) {
	for i, c := range parent.Conditions {
		if c.Type == condType {
			parent.Conditions = append(parent.Conditions[:i], parent.Conditions[i+1:]...)
			return
		}
	}
}

// setRouteCondition sets or updates a condition in the RouteParentStatus.
func setRouteCondition(parent *gatewayv1.RouteParentStatus, condType string, status metav1.ConditionStatus, reason, message string, generation int64, now metav1.Time) {
	for i, c := range parent.Conditions {
		if c.Type == condType {
			if c.Status != status {
				parent.Conditions[i].LastTransitionTime = now
			}
			parent.Conditions[i].Status = status
			parent.Conditions[i].Reason = reason
			parent.Conditions[i].Message = message
			parent.Conditions[i].ObservedGeneration = generation
			return
		}
	}
	parent.Conditions = append(parent.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}
