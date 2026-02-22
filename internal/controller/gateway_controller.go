// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fluxcd/pkg/ssa"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

// DefaultCloudflaredImage is the default cloudflared container image.
// This value is updated automatically by the upgrade-cloudflared workflow.
const DefaultCloudflaredImage = "ghcr.io/matheuscscp/cloudflare-gateway-controller/cloudflared:2026.2.0@sha256:404528c1cd63c3eb882c257ae524919e4376115e6fe57befca8d603656a91a4c"

// ssaApplyOptions configures Server-Side Apply to recreate objects with
// immutable field changes and to clean up field managers used by kubectl
// to force use of GitOps. The controller is the sole owner of objects it
// creates, and offers declarative ways to customize their configuration
// via JSON patches.
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

// gatewayReadiness holds the computed Programmed and Ready condition values
// derived from the cloudflared Deployment status.
type gatewayReadiness struct {
	readyStatus, programmedStatus   metav1.ConditionStatus
	readyReason, readyMsg           string
	programmedReason, programmedMsg string
}

// lbMode represents the load balancer topology mode.
type lbMode int

const (
	lbModeNone          lbMode = iota // Simple: 1 tunnel, CNAMEs
	lbModePerAZ                       // HighAvailability: 1 tunnel per AZ, full ingress
	lbModePerBackendRef               // TrafficSplitting: 1 tunnel per service [× AZ]
)

// tunnelEntry represents one desired tunnel and its associated Kubernetes resources.
type tunnelEntry struct {
	tunnelName     string // Cloudflare tunnel name (globally unique via Gateway UID)
	deploymentName string // cloudflared Deployment name
	secretName     string // tunnel token Secret name
	azName         string // AZ name ("" if no AZ)
	serviceName    string // Service name ("" if not per-backendRef)
	tunnelID       string // filled after ensureTunnels
}

// +kubebuilder:rbac:groups=cloudflare-gateway-controller.io,resources=cloudflaregatewayparameters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudflare-gateway-controller.io,resources=cloudflaregatewaystatuses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudflare-gateway-controller.io,resources=cloudflaregatewaystatuses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// maxEventMessageLen is the maximum length of a Kubernetes Event message.
const maxEventMessageLen = 1024

// truncateEventMessage truncates msg to fit within the Kubernetes Event
// message length limit, appending an ellipsis suffix when truncation occurs.
func truncateEventMessage(msg string) string {
	if len(msg) <= maxEventMessageLen {
		return msg
	}
	const suffix = " ... (truncated)"
	return msg[:maxEventMessageLen-len(suffix)] + suffix
}

// Reconcile handles a single Gateway reconciliation request. It validates the
// GatewayClass, handles deletion via finalize(), adds the finalizer, and
// delegates to reconcile() for the main reconciliation logic.
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

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
		l.V(1).Info("Added finalizer to Gateway")
		return ctrl.Result{RequeueAfter: 1}, nil
	}

	// Skip reconciliation if the GatewayClass is not ready (e.g. incompatible
	// CRD version). The GatewayClass watch will re-trigger when it becomes ready.
	if ready := conditions.Find(gc.Status.Conditions, apiv1.ConditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		l.V(1).Info("GatewayClass is not ready, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Skip reconciliation if the object is suspended.
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		l.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	l.V(1).Info("Reconciling Gateway")

	return r.reconcile(ctx, &gw, &gc)
}

// reconcile orchestrates the main reconciliation loop for a Gateway:
// ensure GatewayClass finalizer, validate, list HTTPRoutes, read credentials,
// create Cloudflare client, detect LB mode, filter and validate routes,
// compute desired tunnels, ensure tunnels and associated Secrets/Deployments,
// clean up stale resources, reconcile tunnel ingress and DNS, check readiness,
// update HTTPRoute and Gateway statuses.
func (r *GatewayReconciler) reconcile(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	// Ensure GatewayClass finalizer
	if err := r.ensureGatewayClassFinalizer(ctx, gc, gw); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("ensuring GatewayClass finalizer: %w", err))
	}

	// Validate Gateway spec.
	if err := validateGateway(gw); err != nil {
		return r.reconcileError(ctx, gw, err.err, err.cond)
	}

	// List all HTTPRoutes referencing this Gateway once, then reuse the list
	// for attached route counts, ingress rules, DNS, and status updates.
	var allRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &allRoutes, client.MatchingFields{
		indexHTTPRouteParentGateway: gw.Namespace + "/" + gw.Name,
	}); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("listing HTTPRoutes: %w", err))
	}

	// Resolve CloudflareGatewayParameters if referenced.
	params, err := readParameters(ctx, r.Client, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("reading parameters: %w", err),
			metav1.Condition{
				Type:    string(gatewayv1.GatewayConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.GatewayReasonInvalidParameters),
				Message: fmt.Sprintf("Failed to read parameters: %v", err),
			})
	}

	// Validate parameters (defense-in-depth, mirrors CEL XValidation rules).
	if err := validateParameters(params); err != nil {
		return r.reconcileError(ctx, gw, err.err, err.cond)
	}

	// Read credentials
	cfg, err := readCredentials(ctx, r.Client, gc, gw, params)
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

	// Accumulators for resource changes and non-fatal errors, summarized in
	// the final status patch and event.
	var changes []string
	var errs []string

	// Detect LB topology mode.
	mode := detectLBMode(params)

	// Filter HTTPRoutes by parentRef and allowedRoutes. Done before tunnel
	// operations because per-backendRef mode needs the valid routes to
	// discover which Services need tunnels.
	gatewayRoutes, deniedRoutes, staleRoutes, err := listGatewayRoutes(ctx, r.Client, &allRoutes, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	// Remove stale status.parents entries for routes whose parentRef was removed.
	for _, route := range staleRoutes {
		if err := r.removeRouteStatus(ctx, gw, route); err != nil {
			errs = append(errs, fmt.Sprintf("failed to remove stale HTTPRoute %s/%s status: %v", route.Namespace, route.Name, err))
		}
	}
	// Set Accepted=False/NotAllowedByListeners on denied routes (cross-namespace
	// route not permitted by the Gateway's listener allowedRoutes).
	for _, route := range deniedRoutes {
		if err := r.updateDeniedRouteStatus(ctx, gw, route); err != nil {
			errs = append(errs, fmt.Sprintf("failed to update denied HTTPRoute %s/%s status: %v", route.Namespace, route.Name, err))
		}
	}
	// Set Accepted=False/UnsupportedValue on routes that use unsupported features.
	var validRoutes []*gatewayv1.HTTPRoute
	for _, route := range gatewayRoutes {
		if issues := validateHTTPRoute(route, mode); len(issues) > 0 {
			if err := r.updateInvalidRouteStatus(ctx, gw, route, issues); err != nil {
				errs = append(errs, fmt.Sprintf("failed to update invalid HTTPRoute %s/%s status: %v", route.Namespace, route.Name, err))
			}
			continue
		}
		validRoutes = append(validRoutes, route)
	}

	// Compute desired tunnel entries based on mode.
	entries := computeDesiredTunnels(gw, mode, params, validRoutes)

	// Ensure all tunnels exist.
	tunnelChanges, err := r.ensureTunnels(ctx, tc, entries)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, tunnelChanges...)

	// Reconcile tunnel token Secrets for all entries.
	secretChanges, err := r.reconcileTunnelTokenSecrets(ctx, gw, tc, entries)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, secretChanges...)

	// Apply cloudflared Deployments for all entries.
	deployChanges, err := r.applyCloudflaredDeployments(ctx, gw, params, entries)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, deployChanges...)

	// Clean up stale tunnel resources (from removed AZs, services, or mode switches).
	cleanupChanges, cleanupErrs := r.cleanupStaleTunnelResources(ctx, tc, gw, entries)
	changes = append(changes, cleanupChanges...)
	errs = append(errs, cleanupErrs...)

	// Build listener statuses using only valid routes.
	desiredListeners := buildListenerStatuses(gw, countAttachedRoutes(validRoutes, gw))

	// Reconcile tunnel ingress configuration for all entries.
	routesWithDeniedRefs, configChanges, err := r.reconcileAllTunnelIngress(ctx, tc, mode, entries, validRoutes)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, configChanges...)

	// Read existing CGS for previous zone info (needed for cleanup on mode/zone changes).
	cgs, cgsErr := r.getCGS(ctx, gw)
	if cgsErr != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("getting CloudflareGatewayStatus: %w", cgsErr))
	}

	// Reconcile DNS or Load Balancer depending on mode, handling zone changes.
	var zoneName string
	if params != nil && params.Spec.DNS != nil {
		zoneName = params.Spec.DNS.Zone.Name
	}
	dnsLB := r.reconcileDNSOrLB(ctx, tc, gw, params, cgs, mode, entries, validRoutes, zoneName)
	changes = append(changes, dnsLB.changes...)
	errs = append(errs, dnsLB.errs...)

	// Check readiness of all cloudflared Deployments.
	readiness := r.checkAllDeploymentsReadiness(ctx, gw, entries)

	// Update HTTPRoute status.parents for allowed routes (after DNS and
	// Deployment checks so we can report DNS status and Gateway readiness).
	errs = append(errs, r.updateRouteStatuses(ctx, gw, validRoutes, routesWithDeniedRefs, zoneName, dnsLB.dnsErr,
		readiness.readyStatus, readiness.readyReason, readiness.readyMsg)...)

	// Reconcile the CloudflareGatewayStatus resource state (tunnels, LB, DNS).
	// Done before the Gateway status patch so CGS reflects the latest state
	// even if the status patch fails.
	if updatedCGS, err := r.reconcileCGS(ctx, gw, cgs, entries, zoneName, dnsLB.lbState); err != nil {
		errs = append(errs, fmt.Sprintf("failed to reconcile CloudflareGatewayStatus: %v", err))
	} else {
		cgs = updatedCGS
	}

	// Patch Gateway status and emit events
	return r.patchGatewayStatus(ctx, gw, cgs, entries, desiredListeners, changes, errs, readiness)
}

// dnsLBResult holds the results of DNS/LB reconciliation.
type dnsLBResult struct {
	lbState lbReconcileState
	dnsErr  *string
	changes []string
	errs    []string
}

// reconcileDNSOrLB reconciles DNS or Load Balancer resources depending on the
// LB mode, handling zone changes between the previous CGS state and current
// parameters.
func (r *GatewayReconciler) reconcileDNSOrLB(
	ctx context.Context, tc cloudflare.Client,
	gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters,
	cgs *apiv1.CloudflareGatewayStatus, mode lbMode,
	entries []tunnelEntry, validRoutes []*gatewayv1.HTTPRoute,
	zoneName string,
) dnsLBResult {
	var res dnsLBResult

	// Detect zone changes from CGS state — used to clean up DNS/LB
	// resources in the old zone before reconciling in the new one.
	cgsZoneName, cgsZoneID := cgsZoneInfo(cgs)
	zoneChanged := cgsZoneName != "" && cgsZoneName != zoneName

	if mode == lbModeNone {
		// If zone changed to a different non-empty zone, clean up DNS
		// CNAMEs in the old zone first. When zone is removed entirely
		// (zoneName==""), reconcileDNS handles cleanup via cleanupAllDNS.
		if zoneChanged && zoneName != "" && len(entries) > 0 {
			oldDNSChanges, oldDNSErr := r.cleanupDNSInZone(ctx, tc, entries[0].tunnelID, cgsZoneName, cgsZoneID)
			res.changes = append(res.changes, oldDNSChanges...)
			if oldDNSErr != nil {
				res.errs = append(res.errs, fmt.Sprintf("failed to clean up DNS in old zone %s: %v", cgsZoneName, oldDNSErr))
			}
		}
		var tunnelID string
		if len(entries) > 0 {
			tunnelID = entries[0].tunnelID
		}
		dnsChanges, dnsErr := r.reconcileDNS(ctx, tc, tunnelID, zoneName, validRoutes)
		res.changes = append(res.changes, dnsChanges...)
		res.dnsErr = dnsErr
		if dnsErr != nil {
			res.errs = append(res.errs, *dnsErr)
		}
		// Clean up stale LB resources if switching FROM LB mode.
		// Use CGS zone info (from previous reconciliation) since the current
		// params may reference a different zone or no zone at all.
		cleanupZoneName := cgsZoneName
		cleanupZoneID := cgsZoneID
		if cleanupZoneName == "" && zoneName != "" {
			// Fallback to current params if CGS has no zone info (first reconcile
			// or CGS not yet populated).
			cleanupZoneName = zoneName
		}
		lbCleanupChanges, lbCleanupErrs := r.cleanupAllLBResources(ctx, tc, gw, cleanupZoneName, cleanupZoneID)
		res.changes = append(res.changes, lbCleanupChanges...)
		res.errs = append(res.errs, lbCleanupErrs...)
	} else {
		// If zone changed, clean up LB resources in the old zone before
		// reconciling in the new zone.
		if zoneChanged {
			oldZoneID := cgsZoneID
			if oldZoneID == "" {
				if id, err := tc.FindZoneIDByHostname(ctx, cgsZoneName); err != nil {
					res.errs = append(res.errs, fmt.Sprintf("failed to find old zone ID for LB cleanup: %v", err))
				} else {
					oldZoneID = id
				}
			}
			if oldZoneID != "" {
				oldLBChanges, oldLBErrs := r.cleanupAllLBResources(ctx, tc, gw, cgsZoneName, oldZoneID)
				res.changes = append(res.changes, oldLBChanges...)
				res.errs = append(res.errs, oldLBErrs...)
			}
		}
		var lbChanges, lbErrs []string
		res.lbState, lbChanges, lbErrs = r.reconcileLoadBalancer(ctx, tc, gw, params, mode, entries, validRoutes)
		res.changes = append(res.changes, lbChanges...)
		res.errs = append(res.errs, lbErrs...)
		// Clean up stale DNS if switching FROM simple mode.
		if len(entries) > 0 {
			dnsCleanupChanges, dnsCleanupErr := r.cleanupAllDNS(ctx, tc, entries[0].tunnelID)
			res.changes = append(res.changes, dnsCleanupChanges...)
			if dnsCleanupErr != nil {
				res.errs = append(res.errs, fmt.Sprintf("failed to clean up DNS: %v", dnsCleanupErr))
			}
		}
	}
	return res
}

// patchGatewayStatus builds the desired Gateway conditions from the readiness
// state, checks whether a status patch is needed (conditions, listeners, or
// resource changes), patches the status, and emits summary events/logs.
// It also patches the CGS conditions to mirror the Gateway conditions.
func (r *GatewayReconciler) patchGatewayStatus(ctx context.Context, gw *gatewayv1.Gateway, cgs *apiv1.CloudflareGatewayStatus, entries []tunnelEntry, desiredListeners []gatewayv1.ListenerStatus, changes, errs []string, readiness gatewayReadiness) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	now := metav1.Now()

	// Downgrade Ready to Unknown/ProgressingWithRetry when there are non-fatal
	// errors, since we will return the error to controller-runtime for retry.
	// Terminal failures (Ready=False) take precedence over transient errors.
	if len(errs) > 0 && readiness.readyStatus != metav1.ConditionFalse {
		readiness.readyStatus = metav1.ConditionUnknown
		readiness.readyReason = apiv1.ReasonProgressingWithRetry
		readiness.readyMsg = strings.Join(errs, "; ")
	}

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
			Status:             readiness.programmedStatus,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             readiness.programmedReason,
			Message:            readiness.programmedMsg,
		},
		{
			Type:               apiv1.ConditionReady,
			Status:             readiness.readyStatus,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             readiness.readyReason,
			Message:            readiness.readyMsg,
		},
	}

	desiredAddresses := make([]gatewayv1.GatewayStatusAddress, 0, len(entries))
	for _, e := range entries {
		desiredAddresses = append(desiredAddresses, gatewayv1.GatewayStatusAddress{
			Type:  new(gatewayv1.HostnameAddressType),
			Value: cloudflare.TunnelTarget(e.tunnelID),
		})
	}

	// Check if Ready is transitioning to a terminal failure state (must be
	// computed before conditions.Set mutates gw.Status.Conditions).
	readyTerminal := readiness.readyStatus == metav1.ConditionFalse &&
		conditions.Changed(gw.Status.Conditions, apiv1.ConditionReady, readiness.readyStatus, readiness.readyReason, readiness.readyMsg, gw.Generation)

	// Force-patch when Progressing/ProgressingWithRetry so the resource
	// version bumps, signalling to users that the controller is alive
	// and making progress.
	requeueAfter := apiv1.ReconcileInterval(gw.Annotations)
	forcePatch := readiness.readyReason == apiv1.ReasonProgressing ||
		readiness.readyReason == apiv1.ReasonProgressingWithRetry

	// Check for real status changes (conditions, listeners, addresses).
	var statusChanged bool
	for _, c := range desiredConds {
		if conditions.Changed(gw.Status.Conditions, c.Type, c.Status, c.Reason, c.Message, gw.Generation) {
			statusChanged = true
			break
		}
	}
	if !statusChanged && listenersChanged(gw.Status.Listeners, desiredListeners) {
		statusChanged = true
	}
	if !statusChanged && addressesChanged(gw.Status.Addresses, desiredAddresses) {
		statusChanged = true
	}

	// Skip if nothing changed: no status changes, no force-patch, no
	// infrastructure changes, and no errors.
	if !forcePatch && !statusChanged && len(changes) == 0 && len(errs) == 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if forcePatch || statusChanged {
		patch := client.MergeFrom(gw.DeepCopy())
		gw.Status.Conditions = conditions.Set(gw.Status.Conditions, desiredConds)
		gw.Status.Listeners = desiredListeners
		gw.Status.Addresses = desiredAddresses
		if err := r.Status().Patch(ctx, gw, patch); err != nil {
			if len(errs) > 0 {
				// Best-effort patch failed; log and fall through to
				// return the original reconciliation error.
				l.Error(err, "Best-effort Gateway status patch failed, returning original error")
			} else {
				return ctrl.Result{}, err
			}
		} else {
			l.V(1).Info("Patched Gateway status")
		}

		// Best-effort patch CGS conditions to mirror Gateway conditions.
		// Only patch when conditions actually changed to minimize writes.
		if cgs != nil {
			var cgsCondsChanged bool
			for _, c := range desiredConds {
				if conditions.Changed(cgs.Status.Conditions, c.Type, c.Status, c.Reason, c.Message, gw.Generation) {
					cgsCondsChanged = true
					break
				}
			}
			if cgsCondsChanged || forcePatch {
				cgsPatch := client.MergeFrom(cgs.DeepCopy())
				cgs.Status.Conditions = conditions.Set(cgs.Status.Conditions, desiredConds)
				if err := r.Status().Patch(ctx, cgs, cgsPatch); err != nil {
					l.Error(err, "Failed to patch CloudflareGatewayStatus conditions")
				}
			}
		}
	}

	// Emit log and event summarizing errors and/or resource changes.
	// Terminal transitions (Ready=False) are included as warnings.
	// Status changes (e.g. Ready transitions to True) without infrastructure
	// changes still emit an event so users see the transition.
	var warnings []string
	if readyTerminal {
		warnings = append(warnings, readiness.readyMsg)
	}
	warnings = append(warnings, errs...)
	if len(warnings) > 0 {
		summary := make([]string, 0, len(warnings)+len(changes))
		summary = append(summary, warnings...)
		summary = append(summary, changes...)
		eventReason := apiv1.ReasonProgressingWithRetry
		if readyTerminal {
			eventReason = readiness.readyReason
		}
		l.Error(fmt.Errorf("%s", strings.Join(warnings, "; ")), "Gateway reconciled with errors", "changes", changes)
		r.Eventf(gw, nil, corev1.EventTypeWarning, eventReason,
			apiv1.EventActionReconcile, "%s", truncateEventMessage(strings.Join(summary, "\n")))
	} else if len(changes) > 0 || statusChanged {
		summary := make([]string, 0, len(changes)+1)
		summary = append(summary, changes...)
		if statusChanged {
			summary = append(summary, readiness.readyMsg)
		}
		l.Info("Gateway reconciled", "changes", summary)
		r.Eventf(gw, nil, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded,
			apiv1.EventActionReconcile, "%s", truncateEventMessage(strings.Join(summary, "\n")))
	}

	if len(errs) > 0 {
		return ctrl.Result{}, fmt.Errorf("%s", strings.Join(errs, "; "))
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
	terminal := errors.Is(reconcileErr, reconcile.TerminalError(nil))
	readyStatus := metav1.ConditionUnknown
	readyReason := apiv1.ReasonProgressingWithRetry
	if terminal {
		readyStatus = metav1.ConditionFalse
		readyReason = apiv1.ReasonReconciliationFailed
	}
	msg := reconcileErr.Error()
	now := metav1.Now()
	desiredConds := append([]metav1.Condition{
		{
			Type:               apiv1.ConditionReady,
			Status:             readyStatus,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: now,
			Reason:             readyReason,
			Message:            msg,
		},
	}, extraConds...)
	for i := range desiredConds[1:] {
		desiredConds[i+1].ObservedGeneration = gw.Generation
		desiredConds[i+1].LastTransitionTime = now
	}

	patch := client.MergeFrom(gw.DeepCopy())
	for _, c := range desiredConds {
		gw.Status.Conditions = conditions.Upsert(gw.Status.Conditions, c)
	}
	if err := r.Status().Patch(ctx, gw, patch); err != nil {
		log.FromContext(ctx).Error(err, "Failed to patch Gateway status with error conditions",
			"originalError", msg)
	} else {
		log.FromContext(ctx).V(1).Info("Patched Gateway status with error conditions")
	}

	r.Eventf(gw, nil, corev1.EventTypeWarning, readyReason,
		apiv1.EventActionReconcile, "Reconciliation failed: %v", reconcileErr)
	return ctrl.Result{}, reconcileErr
}

// finalizeError handles finalization errors by best-effort patching Ready=Unknown
// on the Gateway status and emitting a Warning event, mirroring reconcileError
// but for the finalization path. Any changes completed before the error are
// included in the event so they are not lost.
func (r *GatewayReconciler) finalizeError(ctx context.Context, gw *gatewayv1.Gateway, changes []string, finalizeErr error) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	msg := finalizeErr.Error()
	now := metav1.Now()

	patch := client.MergeFrom(gw.DeepCopy())
	gw.Status.Conditions = conditions.Upsert(gw.Status.Conditions, metav1.Condition{
		Type:               apiv1.ConditionReady,
		Status:             metav1.ConditionUnknown,
		ObservedGeneration: gw.Generation,
		LastTransitionTime: now,
		Reason:             apiv1.ReasonProgressingWithRetry,
		Message:            msg,
	})
	if err := r.Status().Patch(ctx, gw, patch); err != nil {
		l.Error(err, "Failed to patch Gateway status with error conditions",
			"originalError", msg)
	} else {
		l.V(1).Info("Patched Gateway status with error conditions")
	}

	summary := make([]string, 0, 1+len(changes))
	summary = append(summary, fmt.Sprintf("Finalization failed: %v", finalizeErr))
	summary = append(summary, changes...)
	l.Error(finalizeErr, "Finalization failed", "changes", changes)
	r.Eventf(gw, nil, corev1.EventTypeWarning, apiv1.ReasonProgressingWithRetry,
		apiv1.EventActionFinalize, "%s", truncateEventMessage(strings.Join(summary, "\n")))
	return ctrl.Result{}, finalizeErr
}

// finalize dispatches to finalizeDisabled or finalizeEnabled based on the
// reconciliation annotation, then removes HTTPRoute status entries, the
// GatewayClass finalizer, emits a "Gateway finalized" event, and removes
// the Gateway finalizer.
func (r *GatewayReconciler) finalize(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var changes []string
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.ValueDisabled {
		if err := r.finalizeDisabled(ctx, gw); err != nil {
			return r.finalizeError(ctx, gw, changes, err)
		}
	} else {
		var err error
		changes, err = r.finalizeEnabled(ctx, gw, gc)
		if err != nil {
			return r.finalizeError(ctx, gw, changes, err)
		}
	}

	// Remove this Gateway's entry from status.parents on all referencing HTTPRoutes.
	if err := r.removeRouteStatuses(ctx, gw); err != nil {
		return r.finalizeError(ctx, gw, changes, fmt.Errorf("removing HTTPRoute status entries: %w", err))
	}
	l.V(1).Info("Removed HTTPRoute status entries")

	// Remove this Gateway's finalizer from all GatewayClasses.
	if err := r.removeGatewayClassFinalizer(ctx, gw); err != nil {
		return r.finalizeError(ctx, gw, changes, fmt.Errorf("removing GatewayClass finalizer: %w", err))
	}
	l.V(1).Info("Removed GatewayClass finalizer")

	l.Info("Gateway finalized", "changes", changes)
	r.Eventf(gw, nil, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded,
		apiv1.EventActionFinalize, "%s", truncateEventMessage("Gateway finalized\n"+strings.Join(changes, "\n")))

	// Remove finalizer from Gateway if needed. Ignore NotFound because a
	// previous finalization may have already removed the finalizer and
	// allowed Kubernetes to delete the object while the informer cache
	// was stale, causing this finalization to run again on the cached copy.
	if controllerutil.ContainsFinalizer(gw, apiv1.Finalizer) {
		gwPatch := client.MergeFrom(gw.DeepCopy())
		controllerutil.RemoveFinalizer(gw, apiv1.Finalizer)
		if err := r.Patch(ctx, gw, gwPatch); client.IgnoreNotFound(err) != nil {
			return r.finalizeError(ctx, gw, changes, fmt.Errorf("removing finalizer: %w", err))
		}
		l.V(1).Info("Removed finalizer from Gateway")
	}

	return ctrl.Result{}, nil
}

// finalizeDisabled removes owner references from managed resources so
// Kubernetes GC doesn't delete them when the Gateway is removed with
// reconciliation disabled. The user is responsible for manual cleanup.
func (r *GatewayReconciler) finalizeDisabled(ctx context.Context, gw *gatewayv1.Gateway) error {
	l := log.FromContext(ctx)

	removed, err := r.removeOwnerReferences(ctx, gw)

	// Log and emit events for removed objects, even on partial failure.
	removedInfo := make([]string, 0, len(removed))
	for _, obj := range removed {
		removedInfo = append(removedInfo, fmt.Sprintf("%s/%s/%s",
			obj.GetObjectKind().GroupVersionKind().Kind,
			obj.GetNamespace(),
			obj.GetName()))
	}
	if len(removedInfo) > 0 {
		l.Info(
			"Finalization: Gateway disabled, removed owner references from managed objects",
			"objects", removedInfo)
	}
	for _, obj := range removed {
		r.Eventf(obj, gw, corev1.EventTypeNormal, apiv1.ReasonReconciliationDisabled,
			apiv1.EventActionFinalize, "Gateway removed owner reference due to disabled finalization")
	}

	if err != nil {
		return fmt.Errorf("removing owner references: %w", err)
	}
	return nil
}

// finalizeEnabled deletes all cloudflared Deployments (waiting for them to be
// fully removed), then reads credentials, cleans up DNS CNAME records across
// all zones, and deletes all Cloudflare tunnels owned by this Gateway.
func (r *GatewayReconciler) finalizeEnabled(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) ([]string, error) {
	l := log.FromContext(ctx)

	// Delete all cloudflared Deployments owned by this Gateway and wait for
	// them to be gone before deleting tunnels, so there are no active connections.
	var deployList appsv1.DeploymentList
	if err := r.List(ctx, &deployList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":       "cloudflared",
			"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
			"app.kubernetes.io/instance":   gw.Name,
		},
	); err != nil {
		return nil, fmt.Errorf("listing cloudflared deployments for deletion: %w", err)
	}
	for i := range deployList.Items {
		deploy := &deployList.Items[i]
		if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("deleting cloudflared deployment %s: %w", deploy.Name, err)
		}
		l.V(1).Info("Deleted cloudflared Deployment", "deployment", deploy.Name)
	}

	// Wait for all Deployments to be gone.
	for i := range deployList.Items {
		deploy := &deployList.Items[i]
		deployKey := client.ObjectKeyFromObject(deploy)
		for {
			if err := r.Get(ctx, deployKey, deploy); apierrors.IsNotFound(err) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("waiting for cloudflared deployment %s deletion: %w", deploy.Name, err)
			}
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("waiting for cloudflared deployment %s deletion: %w", deploy.Name, ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	l.V(1).Info("All cloudflared Deployments are gone")

	// Read CGS for zone info (needed for LB cleanup even if CGP is deleted).
	cgs, cgsErr := r.getCGS(ctx, gw)
	if cgsErr != nil {
		return nil, fmt.Errorf("getting CloudflareGatewayStatus: %w", cgsErr)
	}

	// Try reading params; if CGP was deleted, params will be nil.
	params, err := readParameters(ctx, r.Client, gw)
	if err != nil {
		// If CGP is NotFound (TerminalError), we can still finalize using
		// CGS zone info and GatewayClass credentials.
		if !errors.Is(err, reconcile.TerminalError(nil)) {
			return nil, fmt.Errorf("reading parameters for tunnel deletion: %w", err)
		}
		l.V(1).Info("CloudflareGatewayParameters not found, using CGS zone info for cleanup")
	}
	cfg, err := readCredentials(ctx, r.Client, gc, gw, params)
	if err != nil {
		return nil, fmt.Errorf("reading credentials for tunnel deletion: %w", err)
	}
	tc, err := r.NewCloudflareClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating cloudflare client for deletion: %w", err)
	}

	var changes []string
	for i := range deployList.Items {
		changes = append(changes, fmt.Sprintf("deleted cloudflared Deployment %s", deployList.Items[i].Name))
	}

	// Resolve zone info for LB cleanup. Prefer CGS (persisted from previous
	// reconciliation), fall back to current params.
	cleanupZoneName, cleanupZoneID := cgsZoneInfo(cgs)
	if cleanupZoneName == "" && params != nil && params.Spec.DNS != nil {
		cleanupZoneName = params.Spec.DNS.Zone.Name
		// zoneID not available here; cleanupAllLBResources will resolve it.
	}
	if cleanupZoneName != "" && cleanupZoneID == "" {
		cleanupZoneID, err = tc.FindZoneIDByHostname(ctx, cleanupZoneName)
		if err != nil {
			return changes, fmt.Errorf("finding zone ID for LB cleanup: %w", err)
		}
	}

	// Clean up LB resources before tunnels (LBs reference pools, pools reference monitor).
	// Always attempt cleanup regardless of current topology — the topology may have changed
	// and stale LB resources from a previous config must be removed.
	lbCleanupChanges, lbCleanupErrs := r.cleanupAllLBResources(ctx, tc, gw, cleanupZoneName, cleanupZoneID)
	changes = append(changes, lbCleanupChanges...)
	if len(lbCleanupErrs) > 0 {
		return changes, fmt.Errorf("cleaning up LB resources: %s", strings.Join(lbCleanupErrs, "; "))
	}

	// Delete all tunnels owned by this Gateway (matching "gateway-{UID}" prefix).
	prefix := "gateway-" + string(gw.UID)
	tunnels, err := tc.ListTunnels(ctx)
	if err != nil {
		return changes, fmt.Errorf("listing tunnels for deletion: %w", err)
	}

	for _, t := range tunnels {
		if !strings.HasPrefix(t.Name, prefix) {
			continue
		}

		// Delete DNS CNAME records pointing to this tunnel.
		dnsChanges, dnsErr := r.cleanupAllDNS(ctx, tc, t.ID)
		changes = append(changes, dnsChanges...)
		if dnsErr != nil {
			return changes, fmt.Errorf("cleaning up DNS for tunnel %s: %w", t.Name, dnsErr)
		}

		if err := tc.CleanupTunnelConnections(ctx, t.ID); err != nil {
			return changes, fmt.Errorf("cleaning up connections for tunnel %s: %w", t.Name, err)
		}
		if err := tc.DeleteTunnel(ctx, t.ID); err != nil {
			return changes, fmt.Errorf("deleting tunnel %s: %w", t.Name, err)
		}
		changes = append(changes, fmt.Sprintf("deleted tunnel %s", t.Name))
		l.V(1).Info("Deleted tunnel", "tunnelName", t.Name, "tunnelID", t.ID)
	}

	// Remove the CGS finalizer so it can be garbage-collected after the
	// Gateway is fully deleted (owner reference cascade).
	if cgs != nil && controllerutil.ContainsFinalizer(cgs, apiv1.Finalizer) {
		cgsPatch := client.MergeFrom(cgs.DeepCopy())
		controllerutil.RemoveFinalizer(cgs, apiv1.Finalizer)
		if err := r.Patch(ctx, cgs, cgsPatch); err != nil {
			return changes, fmt.Errorf("removing finalizer from CloudflareGatewayStatus: %w", err)
		}
		l.V(1).Info("Removed finalizer from CloudflareGatewayStatus")
	}

	return changes, nil
}
