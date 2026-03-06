// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	TunnelImage         string
	NewCloudflareClient cloudflare.ClientFactory
}

// gatewayReadiness holds the computed Programmed and Ready condition values
// derived from the tunnel Deployment status.
type gatewayReadiness struct {
	readyStatus, programmedStatus   metav1.ConditionStatus
	readyReason, readyMsg           string
	programmedReason, programmedMsg string
}

// tunnelState holds the Cloudflare tunnel identity for a Gateway.
type tunnelState struct {
	name string // deterministic hash name
	id   string // Cloudflare tunnel ID (filled by ensureTunnel)
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
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=grpcroutes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=grpcroutes/status,verbs=update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
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
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.AnnotationReconcileDisabled {
		l.V(1).Info("Reconciliation is disabled")
		return ctrl.Result{}, nil
	}

	l.V(1).Info("Reconciling Gateway")

	return r.reconcile(ctx, &gw, &gc)
}

// reconcile orchestrates the main reconciliation loop for a Gateway:
// ensure GatewayClass finalizer, validate, list routes (HTTPRoute + GRPCRoute),
// read credentials, create Cloudflare client, filter and validate routes,
// ensure tunnel and associated Secret/Deployment, clean up stale resources,
// reconcile tunnel ingress and DNS, check readiness, update route and Gateway
// statuses.
func (r *GatewayReconciler) reconcile(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) (ctrl.Result, error) {
	// Ensure GatewayClass finalizer
	if err := r.ensureGatewayClassFinalizer(ctx, gc, gw); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("ensuring GatewayClass finalizer: %w", err))
	}

	// Validate Gateway spec.
	if err := validateGateway(gw); err != nil {
		return r.reconcileError(ctx, gw, err.err, err.cond)
	}

	// List all routes (HTTPRoute + GRPCRoute) referencing this Gateway once,
	// then reuse the list for attached route counts, ingress rules, DNS, and
	// status updates.
	gwKey := gw.Namespace + "/" + gw.Name
	var allRoutes []routeObject

	var httpRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRoutes, client.MatchingFields{
		indexHTTPRouteParentGateway: gwKey,
	}); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("listing HTTPRoutes: %w", err))
	}
	for i := range httpRoutes.Items {
		allRoutes = append(allRoutes, &httpRouteObject{route: &httpRoutes.Items[i]})
	}

	var grpcRoutes gatewayv1.GRPCRouteList
	if err := r.List(ctx, &grpcRoutes, client.MatchingFields{
		indexGRPCRouteParentGateway: gwKey,
	}); err != nil {
		return r.reconcileError(ctx, gw, fmt.Errorf("listing GRPCRoutes: %w", err))
	}
	for i := range grpcRoutes.Items {
		allRoutes = append(allRoutes, &grpcRouteObject{route: &grpcRoutes.Items[i]})
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

	// Filter routes by parentRef and allowedRoutes.
	gatewayRoutes, deniedRoutes, staleRoutes, err := listGatewayRoutes(ctx, r.Client, allRoutes, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	// Remove stale status.parents entries for routes whose parentRef was removed.
	for _, route := range staleRoutes {
		if err := r.removeRouteStatus(ctx, gw, route); err != nil {
			errs = append(errs, fmt.Sprintf("failed to remove stale %s %s status: %v", route.routeKind(), route.key(), err))
		}
	}
	// Set Accepted=False/NotAllowedByListeners on denied routes (cross-namespace
	// route not permitted by the Gateway's listener allowedRoutes).
	for _, route := range deniedRoutes {
		if err := r.updateDeniedRouteStatus(ctx, gw, route); err != nil {
			errs = append(errs, fmt.Sprintf("failed to update denied %s %s status: %v", route.routeKind(), route.key(), err))
		}
	}
	// Reject routes where every parentRef to this Gateway specifies a
	// sectionName that doesn't match any listener (NoMatchingParent).
	matchedRoutes, noMatchErrs := r.filterNoMatchingParentRoutes(ctx, gw, gatewayRoutes)
	errs = append(errs, noMatchErrs...)
	// Set Accepted=False/UnsupportedValue on routes that use unsupported features.
	var validRoutes []routeObject
	for _, route := range matchedRoutes {
		if issues := route.validate(); len(issues) > 0 {
			msg := "Unsupported features:\n- " + strings.Join(issues, "\n- ")
			if err := r.rejectRouteStatus(ctx, gw, route,
				string(gatewayv1.RouteReasonUnsupportedValue), msg); err != nil {
				errs = append(errs, fmt.Sprintf("failed to update invalid %s %s status: %v", route.routeKind(), route.key(), err))
			}
			continue
		}
		validRoutes = append(validRoutes, route)
	}
	// Reject routes that conflict on (hostname, pathPrefix) with an earlier route.
	// Uses the existing route ConfigMap as source of truth for ownership.
	validRoutes, conflictErrs, err := r.filterConflictingRoutes(ctx, gw, validRoutes)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	errs = append(errs, conflictErrs...)

	// Compute desired tunnel state and replicas.
	replicas := resolveReplicas(params)
	tunnel := tunnelState{name: apiv1.TunnelName(gw)}

	// Ensure the tunnel exists.
	tunnelChanges, err := r.ensureTunnel(ctx, tc, &tunnel)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, tunnelChanges...)

	// Fetch CGS early so token rotation can read the last rotation timestamps.
	cgs, _ := r.getCGS(ctx, gw)

	// Reconcile the tunnel token Secret (includes token rotation).
	tokenResult, err := r.reconcileTunnelTokenSecret(ctx, gw, tc, tunnel, cgs, params)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, tokenResult.changes...)

	// Reconcile route ConfigMap.
	var routeDeniedRefs map[types.NamespacedName][]string
	routeDeniedRefs, routeCMChanges, err := r.reconcileRouteConfigMap(ctx, gw, validRoutes)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, routeCMChanges...)

	// Reconcile tunnel RBAC (ServiceAccount, Role, RoleBinding for ConfigMap access).
	tunnelRBACChanges, err := r.reconcileTunnelRBAC(ctx, gw)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, tunnelRBACChanges...)

	// Apply tunnel Deployments for all replicas. The token hash is set as a
	// pod template annotation so that token rotation triggers a rolling restart.
	deployChanges, err := r.applyTunnelDeployments(ctx, gw, params, replicas, tokenResult.tokenHash)
	if err != nil {
		return r.reconcileError(ctx, gw, err)
	}
	changes = append(changes, deployChanges...)

	// Reconcile VPAs when autoscaling is enabled for at least one container.
	if autoscalingEnabled(params) {
		vpaChanges, err := r.reconcileVPAs(ctx, gw, params, replicas)
		if err != nil {
			return r.reconcileError(ctx, gw, err)
		}
		changes = append(changes, vpaChanges...)
	}

	// Clean up stale Kubernetes resources (Deployments, Secrets) that are no
	// longer desired. Tunnel names are deterministic, so no previous state needed.
	cleanupChanges, cleanupErrs := r.cleanupStaleTunnelResources(ctx, gw, replicas)
	changes = append(changes, cleanupChanges...)
	errs = append(errs, cleanupErrs...)

	// Build set of desired Deployment names for VPA cleanup.
	desiredDeployNames := make(map[string]struct{}, len(replicas))
	if autoscalingEnabled(params) {
		for _, rep := range replicas {
			desiredDeployNames[apiv1.GatewayReplicaName(gw, rep.Name)] = struct{}{}
		}
	}
	// Clean up stale VPAs. When autoscaling is off, desired set is empty → all VPAs deleted.
	vpaCleanupChanges, vpaCleanupErrs := r.cleanupStaleVPAs(ctx, gw, desiredDeployNames)
	changes = append(changes, vpaCleanupChanges...)
	errs = append(errs, vpaCleanupErrs...)

	// Build listener statuses using only valid routes.
	desiredListeners := buildListenerStatuses(gw, countAttachedRoutes(validRoutes, gw))

	// Build DNS policy from parameters.
	var dns dnsPolicy
	if params == nil || params.Spec.DNS == nil {
		dns = dnsPolicy{enabled: true}
	} else if len(params.Spec.DNS.Zones) > 0 {
		zones := make([]string, len(params.Spec.DNS.Zones))
		for i, z := range params.Spec.DNS.Zones {
			zones[i] = z.Name
		}
		dns = dnsPolicy{enabled: true, zones: zones}
	}

	// Extract health URL hostname for DNS inclusion.
	extraHostnames := extraHealthHostnames(params)

	// Extract hostnames from all valid routes for DNS reconciliation.
	var routeHostnames []gatewayv1.Hostname
	for _, route := range validRoutes {
		routeHostnames = append(routeHostnames, route.hostnames()...)
	}

	// Reconcile DNS, handling zone changes.
	dnsResult := r.reconcileDNSWithZoneChange(ctx, tc, tunnel.id, routeHostnames, dns, extraHostnames)
	changes = append(changes, dnsResult.changes...)
	errs = append(errs, dnsResult.errs...)

	// Check readiness of all tunnel Deployments.
	readiness := r.checkAllDeploymentsReadiness(ctx, gw, replicas)

	// Update route status.parents for allowed routes (after DNS and
	// Deployment checks so we can report DNS status and Gateway readiness).
	errs = append(errs, r.updateRouteStatuses(ctx, gw, validRoutes, routeDeniedRefs, dns, dnsResult.dnsErr,
		readiness.readyStatus, readiness.readyReason, readiness.readyMsg)...)

	// Reconcile the CloudflareGatewayStatus (observability only — tunnel info
	// and mirrored conditions). Done before the Gateway status patch so CGS
	// reflects the latest state even if the status patch fails.
	// Copy annotation values to CGS status and update rotation timestamps.
	if updatedCGS, err := r.reconcileCGS(ctx, gw, cgs, params, tunnel, replicas, tokenResult); err != nil {
		errs = append(errs, fmt.Sprintf("failed to reconcile CloudflareGatewayStatus: %v", err))
	} else {
		cgs = updatedCGS
	}

	// Compute requeue interval.
	requeueAfter, requeueErrs := computeRequeueAfter(gw, cgs, tokenResult)
	errs = append(errs, requeueErrs...)

	// Patch Gateway status and emit events
	return r.patchGatewayStatus(ctx, gw, cgs, tunnel, desiredListeners, changes, errs, readiness, dns, requeueAfter)
}

// computeRequeueAfter computes the requeue interval, taking into account the
// reconcile interval and the time until the next scheduled token rotation.
func computeRequeueAfter(gw *gatewayv1.Gateway, cgs *apiv1.CloudflareGatewayStatus, tokenResult *tokenRotationResult) (time.Duration, []string) {
	var errs []string
	requeueAfter, err := apiv1.ReconcileInterval(gw.Annotations)
	if err != nil {
		errs = append(errs, err.Error())
		requeueAfter = apiv1.DefaultReconcileInterval
	}
	if tokenResult.rotationInterval > 0 && cgs != nil && cgs.Status.LastTokenRotatedAt != "" {
		if t, err := time.Parse(time.RFC3339, cgs.Status.LastTokenRotatedAt); err == nil {
			timeUntilNext := max(tokenResult.rotationInterval-time.Since(t), 0)
			if timeUntilNext < requeueAfter {
				requeueAfter = timeUntilNext
			}
		}
	}
	return requeueAfter, errs
}

// dnsResult holds the results of DNS reconciliation.
type dnsResult struct {
	dnsErr  *string
	changes []string
	errs    []string
}

// reconcileDNSWithZoneChange reconciles DNS CNAME records. reconcileDNS
// lists all records pointing to the tunnel across all account zones and
// diffs against the desired set, so no previous-zone tracking is needed.
func (r *GatewayReconciler) reconcileDNSWithZoneChange(
	ctx context.Context, tc cloudflare.Client,
	tunnelID string, routeHostnames []gatewayv1.Hostname,
	dns dnsPolicy, extraHostnames []string,
) dnsResult {
	var res dnsResult
	dnsChanges, dnsErr := r.reconcileDNS(ctx, tc, tunnelID, dns, routeHostnames, extraHostnames)
	res.changes = append(res.changes, dnsChanges...)
	res.dnsErr = dnsErr
	if dnsErr != nil {
		res.errs = append(res.errs, *dnsErr)
	}
	return res
}

// extraHealthHostnames returns the health URL hostname as a slice for DNS
// inclusion, or nil if no health URL is configured.
func extraHealthHostnames(params *apiv1.CloudflareGatewayParameters) []string {
	if params == nil || params.Spec.Tunnel == nil || params.Spec.Tunnel.Health == nil || params.Spec.Tunnel.Health.URL == "" {
		return nil
	}
	if h := healthURLHostname(params.Spec.Tunnel.Health.URL); h != "" {
		return []string{h}
	}
	return nil
}

// healthURLHostname extracts the hostname from a URL string. Returns ""
// if the URL cannot be parsed.
func healthURLHostname(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// buildDNSManagementCondition builds the DNSManagement condition for the
// Gateway status based on the current DNS policy.
func buildDNSManagementCondition(generation int64, now metav1.Time, dns dnsPolicy) metav1.Condition {
	cond := metav1.Condition{
		Type:               apiv1.ConditionDNSManagement,
		ObservedGeneration: generation,
		LastTransitionTime: now,
	}
	if dns.enabled {
		cond.Status = metav1.ConditionTrue
		cond.Reason = apiv1.ReasonEnabled
		if dns.allZones() {
			cond.Message = "All hostnames"
		} else {
			var msg strings.Builder
			msg.WriteString("Allowed zones:")
			for _, z := range dns.zones {
				fmt.Fprintf(&msg, "\n- %s", z)
			}
			cond.Message = msg.String()
		}
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = apiv1.ReasonDisabled
		cond.Message = "DNS management is disabled"
	}
	return cond
}

// patchGatewayStatus builds the desired Gateway conditions from the readiness
// state, checks whether a status patch is needed (conditions, listeners, or
// resource changes), patches the status, and emits summary events/logs.
// It also patches the CGS conditions to mirror the Gateway conditions.
func (r *GatewayReconciler) patchGatewayStatus(ctx context.Context, gw *gatewayv1.Gateway, cgs *apiv1.CloudflareGatewayStatus, tunnel tunnelState, desiredListeners []gatewayv1.ListenerStatus, changes, errs []string, readiness gatewayReadiness, dns dnsPolicy, requeueAfter time.Duration) (ctrl.Result, error) {
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

	// Build DNSManagement condition.
	dnsCond := buildDNSManagementCondition(gw.Generation, now, dns)

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
		dnsCond,
	}

	desiredAddresses := []gatewayv1.GatewayStatusAddress{{
		Type:  new(gatewayv1.HostnameAddressType),
		Value: cloudflare.TunnelTarget(tunnel.id),
	}}

	// Check if Ready is transitioning to a terminal failure state (must be
	// computed before conditions.Set mutates gw.Status.Conditions).
	readyTerminal := readiness.readyStatus == metav1.ConditionFalse &&
		conditions.Changed(gw.Status.Conditions, apiv1.ConditionReady, readiness.readyStatus, readiness.readyReason, readiness.readyMsg, gw.Generation)

	// Force-patch when Progressing/ProgressingWithRetry so the resource
	// version bumps, signalling to users that the controller is alive
	// and making progress.
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
		// Only log at error level when not returning an error (terminal
		// transitions without transient errors). When len(errs) > 0 the
		// error is returned to controller-runtime, which logs it.
		if len(errs) == 0 {
			l.Error(fmt.Errorf("%s", strings.Join(warnings, "; ")), "Gateway reconciled with errors", "changes", changes)
		}
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

// resolveReplicas returns the effective replica list from the parameters.
// nil (absent) → single replica named "primary" (default).
// [] (explicitly empty) → no replicas (scale to zero).
// Non-empty → use as-is.
func resolveReplicas(params *apiv1.CloudflareGatewayParameters) []apiv1.ReplicaConfig {
	if params == nil || params.Spec.Tunnel == nil || params.Spec.Tunnel.Replicas == nil {
		return []apiv1.ReplicaConfig{{Name: "primary"}}
	}
	return params.Spec.Tunnel.Replicas
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

	// Best-effort: copy annotation values to CGS status and mirror conditions
	// so the CLI wait logic can detect that the controller handled the request
	// even when reconciliation fails before reconcileCGS runs.
	if cgs, _ := r.getCGS(ctx, gw); cgs != nil {
		cgsPatch := client.MergeFrom(cgs.DeepCopy())
		cgs.Status.LastHandledReconcileAt = gw.Annotations[apiv1.AnnotationReconcileRequestedAt]
		cgs.Status.LastHandledTokenRotateAt = gw.Annotations[apiv1.AnnotationRotateTokenRequestedAt]
		for _, c := range desiredConds {
			cgs.Status.Conditions = conditions.Upsert(cgs.Status.Conditions, c)
		}
		if err := r.Status().Patch(ctx, cgs, cgsPatch); err != nil {
			log.FromContext(ctx).Error(err, "Failed to patch CloudflareGatewayStatus in error path")
		}
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
	// Do not log at error level — the error is returned to
	// controller-runtime, which logs it.
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
	if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.AnnotationReconcileDisabled {
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

	// Remove this Gateway's entry from status.parents on all referencing routes.
	if err := r.removeRouteStatuses(ctx, gw); err != nil {
		return r.finalizeError(ctx, gw, changes, fmt.Errorf("removing route status entries: %w", err))
	}
	l.V(1).Info("Removed route status entries")

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

// finalizeEnabled deletes all tunnel Deployments (waiting for them to be
// fully removed), then reads credentials, cleans up DNS CNAME records across
// all zones, and deletes all Cloudflare tunnels owned by this Gateway.
func (r *GatewayReconciler) finalizeEnabled(ctx context.Context, gw *gatewayv1.Gateway, gc *gatewayv1.GatewayClass) ([]string, error) {
	l := log.FromContext(ctx)

	// Delete all tunnel Deployments owned by this Gateway and wait for
	// them to be gone before deleting tunnels, so there are no active connections.
	var deployList appsv1.DeploymentList
	if err := r.List(ctx, &deployList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name)),
	); err != nil {
		return nil, fmt.Errorf("listing tunnel deployments for deletion: %w", err)
	}
	for i := range deployList.Items {
		deploy := &deployList.Items[i]
		if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("deleting tunnel deployment %s: %w", deploy.Name, err)
		}
		l.V(1).Info("Deleted tunnel Deployment", "deployment", deploy.Name)
	}

	// Wait for all Deployments to be gone.
	for i := range deployList.Items {
		deploy := &deployList.Items[i]
		deployKey := client.ObjectKeyFromObject(deploy)
		for {
			if err := r.Get(ctx, deployKey, deploy); apierrors.IsNotFound(err) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("waiting for tunnel deployment %s deletion: %w", deploy.Name, err)
			}
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("waiting for tunnel deployment %s deletion: %w", deploy.Name, ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	l.V(1).Info("All tunnel Deployments are gone")

	// Try reading params; if CGP was deleted, params will be nil.
	params, err := readParameters(ctx, r.Client, gw)
	if err != nil {
		if !errors.Is(err, reconcile.TerminalError(nil)) {
			return nil, fmt.Errorf("reading parameters for tunnel deletion: %w", err)
		}
		l.V(1).Info("CloudflareGatewayParameters not found, continuing with GatewayClass credentials")
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
		changes = append(changes, fmt.Sprintf("deleted tunnel Deployment %s", deployList.Items[i].Name))
	}

	// Look up the tunnel by its deterministic name.
	tunnelName := apiv1.TunnelName(gw)
	tunnelID, err := tc.GetTunnelIDByName(ctx, tunnelName)
	if err != nil {
		return changes, fmt.Errorf("looking up tunnel %s: %w", tunnelName, err)
	}
	if tunnelID != "" {
		// Delete DNS CNAME records pointing to this tunnel.
		dnsChanges, dnsErr := r.reconcileDNS(ctx, tc, tunnelID, dnsPolicy{}, nil, nil)
		changes = append(changes, dnsChanges...)
		if dnsErr != nil {
			return changes, fmt.Errorf("cleaning up DNS for tunnel %s: %s", tunnelName, *dnsErr)
		}

		if err := tc.CleanupTunnelConnections(ctx, tunnelID); err != nil {
			return changes, fmt.Errorf("cleaning up connections for tunnel %s: %w", tunnelName, err)
		}
		if err := tc.DeleteTunnel(ctx, tunnelID); err != nil {
			return changes, fmt.Errorf("deleting tunnel %s: %w", tunnelName, err)
		}
		changes = append(changes, fmt.Sprintf("deleted tunnel %s", tunnelName))
		l.V(1).Info("Deleted tunnel", "tunnelName", tunnelName, "tunnelID", tunnelID)
	}

	// Remove the CGS finalizer so it can be garbage-collected after the
	// Gateway is fully deleted (owner reference cascade).
	cgs, _ := r.getCGS(ctx, gw)
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
