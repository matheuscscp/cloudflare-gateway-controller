// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/fluxcd/pkg/ssa"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	acappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	accorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	rbacv1 "k8s.io/api/rbac/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/proxy"
)

// ensureTunnel creates or looks up the desired tunnel, filling in the
// tunnel.id field. Returns a list of change messages for created tunnels.
func (r *GatewayReconciler) ensureTunnel(ctx context.Context, tc cloudflare.Client, tunnel *tunnelState) ([]string, error) {
	l := log.FromContext(ctx)

	tunnelID, err := tc.GetTunnelIDByName(ctx, tunnel.name)
	if err != nil {
		return nil, fmt.Errorf("looking up tunnel %q: %w", tunnel.name, err)
	}
	if tunnelID != "" {
		tunnel.id = tunnelID
		return nil, nil
	}

	tunnelID, err = tc.CreateTunnel(ctx, tunnel.name)
	if err != nil {
		if !cloudflare.IsConflict(err) {
			return nil, fmt.Errorf("creating tunnel %q: %w", tunnel.name, err)
		}
		tunnelID, err = tc.GetTunnelIDByName(ctx, tunnel.name)
		if err != nil {
			return nil, fmt.Errorf("looking up tunnel %q after conflict: %w", tunnel.name, err)
		}
	}
	tunnel.id = tunnelID
	l.V(1).Info("Created tunnel", "tunnelName", tunnel.name, "tunnelID", tunnelID)
	return []string{fmt.Sprintf("created tunnel %s", tunnel.name)}, nil
}

// tokenRotationResult holds the results of token rotation check and execution.
type tokenRotationResult struct {
	changes          []string
	lastRotatedAt    string // set when a rotation was performed
	rotationInterval time.Duration
}

// reconcileTunnelTokenSecret reconciles the tunnel token Secret,
// performing token rotation if needed (on-demand or scheduled), and
// setting the Gateway as the controller owner reference. Returns change messages.
func (r *GatewayReconciler) reconcileTunnelTokenSecret(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	tc cloudflare.Client,
	tunnel tunnelState,
	cgs *apiv1.CloudflareGatewayStatus,
	params *apiv1.CloudflareGatewayParameters,
) (*tokenRotationResult, error) {
	l := log.FromContext(ctx)
	result := &tokenRotationResult{}
	resourceName := apiv1.GatewayResourceName(gw)

	// Check if token rotation is needed.
	rotated, err := r.maybeRotateToken(ctx, gw, tc, tunnel, cgs, params, result)
	if err != nil {
		return nil, err
	}

	// Fetch the (possibly new) token.
	tunnelToken, err := tc.GetTunnelToken(ctx, tunnel.id)
	if err != nil {
		return nil, fmt.Errorf("getting tunnel token for %q: %w", tunnel.name, err)
	}
	secret := buildTunnelTokenSecret(gw, tunnelToken)
	if err := controllerutil.SetControllerReference(gw, secret, r.Scheme()); err != nil {
		return nil, fmt.Errorf("setting owner reference on secret %q: %w", resourceName, err)
	}
	createOrUpdateResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		desired := buildTunnelTokenSecret(gw, tunnelToken)
		secret.Data = desired.Data
		secret.Labels = desired.Labels
		secret.Annotations = desired.Annotations
		return controllerutil.SetControllerReference(gw, secret, r.Scheme())
	})
	if err != nil {
		return nil, fmt.Errorf("creating/updating tunnel token secret %q: %w", resourceName, err)
	}
	if createOrUpdateResult != controllerutil.OperationResultNone {
		result.changes = append(result.changes, fmt.Sprintf("tunnel token Secret %s %s", resourceName, createOrUpdateResult))
	}
	l.V(1).Info("Reconciled tunnel token Secret", "secret", resourceName, "result", createOrUpdateResult, "rotated", rotated)
	return result, nil
}

// maybeRotateToken checks if token rotation is needed (on-demand or scheduled)
// and performs it if so.
func (r *GatewayReconciler) maybeRotateToken(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	tc cloudflare.Client,
	tunnel tunnelState,
	cgs *apiv1.CloudflareGatewayStatus,
	params *apiv1.CloudflareGatewayParameters,
	result *tokenRotationResult,
) (bool, error) {
	// Determine rotation config from parameters.
	// Default: enabled=true, interval=24h — rotation is on by default even
	// without a CGP or without any rotation fields set.
	rotationEnabled := true
	rotationInterval := 24 * time.Hour
	if params != nil && params.Spec.Tunnel != nil && params.Spec.Tunnel.Token != nil && params.Spec.Tunnel.Token.Rotation != nil {
		rot := params.Spec.Tunnel.Token.Rotation
		if rot.Enabled != nil {
			rotationEnabled = *rot.Enabled
		}
		if rot.Interval.Duration > 0 {
			rotationInterval = rot.Interval.Duration
		}
	}
	if rotationEnabled {
		result.rotationInterval = rotationInterval
	}

	var lastHandledRotateAt, lastTokenRotatedAt string
	if cgs != nil {
		lastHandledRotateAt = cgs.Status.LastHandledTokenRotateAt
		lastTokenRotatedAt = cgs.Status.LastTokenRotatedAt
	}

	// On-demand rotation: annotation differs from last handled value.
	requestedAt := gw.Annotations[apiv1.AnnotationRotateTokenRequestedAt]
	onDemand := requestedAt != "" && requestedAt != lastHandledRotateAt

	// Scheduled rotation: time since last rotation exceeds interval (only when enabled).
	scheduled := false
	if rotationEnabled {
		if lastTokenRotatedAt != "" {
			if t, err := time.Parse(time.RFC3339, lastTokenRotatedAt); err == nil {
				scheduled = time.Since(t) >= rotationInterval
			}
		} else {
			// First rotation: no last rotation recorded.
			scheduled = true
		}
	}

	if !onDemand && !scheduled {
		return false, nil
	}

	// Generate 32 random bytes.
	newSecret := make([]byte, 32)
	if _, err := rand.Read(newSecret); err != nil {
		return false, fmt.Errorf("generating random secret: %w", err)
	}

	// Rotate the tunnel secret.
	if err := tc.RotateTunnelSecret(ctx, tunnel.id, newSecret); err != nil {
		return false, fmt.Errorf("rotating tunnel secret for %q: %w", tunnel.name, err)
	}

	result.lastRotatedAt = time.Now().Format(time.RFC3339)
	result.changes = append(result.changes, "rotated tunnel token")
	return true, nil
}

// applyTunnelDeployments builds and applies a tunnel Deployment for each
// replica via Server-Side Apply. Returns change messages.
func (r *GatewayReconciler) applyTunnelDeployments(ctx context.Context, gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters, replicas []apiv1.ReplicaConfig) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string
	for i := range replicas {
		deploymentName := apiv1.GatewayReplicaName(gw, replicas[i].Name)
		deployObj, err := r.buildTunnelDeployment(gw, params, &replicas[i])
		if err != nil {
			return nil, fmt.Errorf("building tunnel deployment %q: %w", deploymentName, err)
		}
		ssaEntry, err := r.ResourceManager.Apply(ctx, deployObj, ssaApplyOptions)
		if err != nil {
			return nil, fmt.Errorf("applying tunnel deployment %q: %w", deploymentName, err)
		}
		if string(ssaEntry.Action) != string(ssa.UnchangedAction) {
			changes = append(changes, fmt.Sprintf("tunnel Deployment %s %s", deploymentName, ssaEntry.Action))
		}
		l.V(1).Info("Reconciled tunnel Deployment", "deployment", deploymentName, "action", ssaEntry.Action)
	}
	return changes, nil
}

// checkAllDeploymentsReadiness fetches all tunnel Deployments for the
// replicas and inspects their status to determine the Gateway's
// Programmed and Ready state. All Deployments must be available for the
// Gateway to be considered Programmed.
func (r *GatewayReconciler) checkAllDeploymentsReadiness(ctx context.Context, gw *gatewayv1.Gateway, replicas []apiv1.ReplicaConfig) gatewayReadiness {
	l := log.FromContext(ctx)
	allAvailable := true
	anyDeadlineExceeded := false
	var notReadyNames []string

	for i := range replicas {
		deploymentName := apiv1.GatewayReplicaName(gw, replicas[i].Name)
		var deploy appsv1.Deployment
		deployAvailable := false
		deployDeadlineExceeded := false
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: gw.Namespace,
			Name:      deploymentName,
		}, &deploy); err == nil {
			for _, c := range deploy.Status.Conditions {
				switch {
				case c.Type == appsv1.DeploymentAvailable && c.Status == "True":
					deployAvailable = true
				case c.Type == appsv1.DeploymentProgressing && c.Status == "False":
					deployDeadlineExceeded = true
				}
			}
		} else {
			l.V(1).Info("Failed to get Deployment, treating as unavailable", "deployment", deploymentName, "error", err)
		}
		if deployDeadlineExceeded {
			anyDeadlineExceeded = true
			notReadyNames = append(notReadyNames, deploymentName)
		} else if !deployAvailable {
			allAvailable = false
			notReadyNames = append(notReadyNames, deploymentName)
		}
	}

	readiness := gatewayReadiness{
		readyStatus:      metav1.ConditionUnknown,
		readyReason:      apiv1.ReasonProgressingWithRetry,
		programmedStatus: metav1.ConditionFalse,
		programmedReason: string(gatewayv1.GatewayReasonPending),
		programmedMsg:    "Waiting for tunnel deployment to become ready",
	}
	if anyDeadlineExceeded {
		readiness.readyStatus = metav1.ConditionFalse
		readiness.readyReason = apiv1.ReasonReconciliationFailed
		readiness.readyMsg = fmt.Sprintf("tunnel deployment(s) exceeded progress deadline: %s", strings.Join(notReadyNames, ", "))
	} else if allAvailable {
		readiness.programmedStatus = metav1.ConditionTrue
		readiness.programmedReason = string(gatewayv1.GatewayReasonProgrammed)
		readiness.programmedMsg = "Gateway is programmed"
		readiness.readyStatus = metav1.ConditionTrue
		readiness.readyReason = apiv1.ReasonReconciliationSucceeded
		readiness.readyMsg = "Gateway is ready"
	} else {
		readiness.readyReason = apiv1.ReasonProgressing
		readiness.readyMsg = fmt.Sprintf("Waiting for tunnel deployment(s) to become ready: %s", strings.Join(notReadyNames, ", "))
	}
	return readiness
}

// removeOwnerReferences removes the Gateway's owner references from all
// managed tunnel Deployments and tunnel token Secrets so they survive
// garbage collection when the Gateway is deleted with reconciliation disabled.
// Returns the list of resources that were modified.
func (r *GatewayReconciler) removeOwnerReferences(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object
	matchLabels := client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name))

	// Remove owner references from all tunnel Deployments.
	var deployList appsv1.DeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing Deployments: %w", err)
	}
	for i := range deployList.Items {
		deploy := &deployList.Items[i]
		deployKey := client.ObjectKeyFromObject(deploy)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, deployKey, deploy); err != nil {
				return client.IgnoreNotFound(err)
			}
			deployPatch := client.MergeFromWithOptions(deploy.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(deploy, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, deploy, deployPatch); err != nil {
				return fmt.Errorf("patching deployment %s: %w", deploy.Name, err)
			}
			deploy.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(apiv1.KindDeployment))
			removed = append(removed, deploy)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from Deployment", "deployment", deployKey)
	}

	// Remove owner references from all tunnel token Secrets.
	var secretList corev1.SecretList
	if err := r.List(ctx, &secretList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing Secrets: %w", err)
	}
	for i := range secretList.Items {
		secret := &secretList.Items[i]
		secretKey := client.ObjectKeyFromObject(secret)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, secretKey, secret); err != nil {
				return client.IgnoreNotFound(err)
			}
			secretPatch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(secret, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, secret, secretPatch); err != nil {
				return fmt.Errorf("patching secret %s: %w", secret.Name, err)
			}
			secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(apiv1.KindSecret))
			removed = append(removed, secret)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from Secret", "secret", secretKey)
	}

	// Remove owner references from route ConfigMap.
	routeCMRemoved, err := r.removeOwnerReferencesFromRouteConfigMap(ctx, gw)
	removed = append(removed, routeCMRemoved...)
	if err != nil {
		return removed, err
	}

	// Remove owner references from tunnel RBAC resources.
	tunnelRBACRemoved, err := r.removeOwnerReferencesFromTunnelRBACResources(ctx, gw)
	removed = append(removed, tunnelRBACRemoved...)
	if err != nil {
		return removed, err
	}

	// Remove owner references from VPAs.
	vpaRemoved, err := r.removeOwnerReferencesFromVPAs(ctx, gw)
	removed = append(removed, vpaRemoved...)
	if err != nil {
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

// cleanupStaleTunnelResources deletes Deployments and Secrets
// that are no longer part of the desired replicas. This handles replica
// addition/removal, service changes, and mode switches.
func (r *GatewayReconciler) cleanupStaleTunnelResources(ctx context.Context, gw *gatewayv1.Gateway, replicas []apiv1.ReplicaConfig) ([]string, []string) {
	l := log.FromContext(ctx)

	// Build set of desired Deployment names.
	desired := make(map[string]struct{}, len(replicas))
	for _, r := range replicas {
		desired[apiv1.GatewayReplicaName(gw, r.Name)] = struct{}{}
	}

	// List all tunnel Deployments owned by this Gateway.
	var deployList appsv1.DeploymentList
	if err := r.List(ctx, &deployList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name)),
	); err != nil {
		return nil, []string{fmt.Sprintf("failed to list tunnel Deployments for cleanup: %v", err)}
	}

	var changes []string
	var errs []string
	for i := range deployList.Items {
		deploy := &deployList.Items[i]
		if _, ok := desired[deploy.Name]; ok {
			continue // still desired
		}

		// Delete the stale Deployment.
		if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Sprintf("failed to delete stale Deployment %s: %v", deploy.Name, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted stale Deployment %s", deploy.Name))
		l.V(1).Info("Deleted stale tunnel Deployment", "deployment", deploy.Name)
	}

	// Delete corresponding stale Secrets (by convention, Secrets owned by the
	// Gateway with the tunnel label pattern). We use owner references
	// to find them via GC, but also clean up explicitly.
	var secretList corev1.SecretList
	if err := r.List(ctx, &secretList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name)),
	); err != nil {
		errs = append(errs, fmt.Sprintf("failed to list tunnel token Secrets for cleanup: %v", err))
		return changes, errs
	}

	desiredSecrets := map[string]struct{}{apiv1.GatewayResourceName(gw): {}}
	for i := range secretList.Items {
		secret := &secretList.Items[i]
		if _, ok := desiredSecrets[secret.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Sprintf("failed to delete stale Secret %s: %v", secret.Name, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted stale Secret %s", secret.Name))
		l.V(1).Info("Deleted stale tunnel token Secret", "secret", secret.Name)
	}

	return changes, errs
}

func buildTunnelTokenSecret(gw *gatewayv1.Gateway, tunnelToken string) *corev1.Secret {
	lbls := apiv1.GatewayResourceLabels(gw.Name)
	maps.Copy(lbls, infrastructureLabels(gw.Spec.Infrastructure))
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        apiv1.GatewayResourceName(gw),
			Namespace:   gw.Namespace,
			Labels:      lbls,
			Annotations: infrastructureAnnotations(gw.Spec.Infrastructure),
		},
		Data: map[string][]byte{
			cloudflare.TunnelTokenSecretKey: []byte(tunnelToken),
		},
	}
}

// buildTunnelDeployment builds the desired tunnel Deployment as an
// unstructured object suitable for server-side apply via the SSA manager.
//
// The build order is:
//  1. Base deployment (tunnel container, no placement)
//  2. User-defined RFC 6902 patches (terminal errors)
//  3. Placement overrides from replica config (affinity, zone, nodeSelector)
//  4. Convert to unstructured
func (r *GatewayReconciler) buildTunnelDeployment(gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters, replica *apiv1.ReplicaConfig) (*unstructured.Unstructured, error) {
	// Step 1: Build the base deployment (no placement).
	apply := r.buildTunnelDeploymentApply(gw, params, replica)
	data, err := json.Marshal(apply)
	if err != nil {
		return nil, fmt.Errorf("marshaling deployment: %w", err)
	}

	// Step 2: Apply RFC 6902 JSON Patch operations from CloudflareGatewayParameters if present.
	// These errors are terminal because the CRD is a watched object and
	// retrying won't fix invalid user input.
	if params != nil && params.Spec.Tunnel != nil && len(params.Spec.Tunnel.Patches) > 0 {
		patchJSON, err := json.Marshal(params.Spec.Tunnel.Patches)
		if err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("marshaling deployment patches: %w", err))
		}
		patch, err := jsonpatch.DecodePatch(patchJSON)
		if err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("decoding deployment patches: %w", err))
		}
		data, err = patch.Apply(data)
		if err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("applying deployment patches: %w", err))
		}
	}

	// Step 3: Apply placement overrides from the replica config.
	data, err = applyPlacementOverrides(data, replica)
	if err != nil {
		return nil, err
	}

	// Step 4: Convert to unstructured.
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("unmarshaling deployment: %w", err)
	}
	obj.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(apiv1.KindDeployment))
	return obj, nil
}

// applyPlacementOverrides applies placement fields (affinity, zone,
// nodeSelector) from the replica config onto the deployment JSON. Placement
// overrides always take priority over user patches.
func applyPlacementOverrides(data []byte, replica *apiv1.ReplicaConfig) ([]byte, error) {
	hasAffinity := replica.Affinity != nil || replica.Zone != ""
	hasNodeSelector := len(replica.NodeSelector) > 0
	if !hasAffinity && !hasNodeSelector {
		return data, nil
	}

	podSpec, err := navigateToPodSpec(data)
	if err != nil {
		return nil, err
	}

	if replica.Affinity != nil {
		affinityJSON, err := json.Marshal(replica.Affinity)
		if err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("marshaling replica affinity: %w", err))
		}
		var affinityMap map[string]any
		if err := json.Unmarshal(affinityJSON, &affinityMap); err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("unmarshaling replica affinity: %w", err))
		}
		podSpec["affinity"] = affinityMap
	} else if replica.Zone != "" {
		affinity := &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "topology.kubernetes.io/zone",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{replica.Zone},
						}},
					}},
				},
			},
		}
		affinityJSON, err := json.Marshal(affinity)
		if err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("marshaling zone affinity: %w", err))
		}
		var affinityMap map[string]any
		if err := json.Unmarshal(affinityJSON, &affinityMap); err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("unmarshaling zone affinity: %w", err))
		}
		podSpec["affinity"] = affinityMap
	}

	if hasNodeSelector {
		// Convert map[string]string to map[string]any for JSON.
		ns := make(map[string]any, len(replica.NodeSelector))
		for k, v := range replica.NodeSelector {
			ns[k] = v
		}
		podSpec["nodeSelector"] = ns
	}

	// Marshal the full deployment back.
	var deploy map[string]any
	if err := json.Unmarshal(data, &deploy); err != nil {
		return nil, reconcile.TerminalError(fmt.Errorf("unmarshaling deployment for placement overrides: %w", err))
	}
	// Re-set the podSpec in the deployment tree.
	spec := deploy["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	template["spec"] = podSpec
	return json.Marshal(deploy)
}

// navigateToPodSpec extracts spec.template.spec from a deployment JSON blob.
func navigateToPodSpec(data []byte) (map[string]any, error) {
	var deploy map[string]any
	if err := json.Unmarshal(data, &deploy); err != nil {
		return nil, reconcile.TerminalError(fmt.Errorf("unmarshaling deployment: %w", err))
	}
	spec, ok := deploy["spec"].(map[string]any)
	if !ok {
		return nil, reconcile.TerminalError(fmt.Errorf("deployment missing spec"))
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		return nil, reconcile.TerminalError(fmt.Errorf("deployment missing spec.template"))
	}
	podSpec, ok := template["spec"].(map[string]any)
	if !ok {
		return nil, reconcile.TerminalError(fmt.Errorf("deployment missing spec.template.spec"))
	}
	return podSpec, nil
}

// buildTunnelDeploymentApply builds the apply configuration for the
// tunnel Deployment, including selector labels, infrastructure
// labels/annotations, owner reference, and the tunnel container spec.
func (r *GatewayReconciler) buildTunnelDeploymentApply(gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters, replica *apiv1.ReplicaConfig) *acappsv1.DeploymentApplyConfiguration {
	deploymentName := apiv1.GatewayReplicaName(gw, replica.Name)
	selectorLabels := apiv1.GatewayResourceLabels(gw.Name, replica.Name)
	templateLabels := maps.Clone(selectorLabels)

	infraLabels := infrastructureLabels(gw.Spec.Infrastructure)
	deployAnnotations := infrastructureAnnotations(gw.Spec.Infrastructure)
	maps.Copy(templateLabels, infraLabels)
	templateAnnotations := infrastructureAnnotations(gw.Spec.Infrastructure)

	// Deployment metadata gets both infrastructure labels and selector labels
	// so the controller can list Deployments by label for cleanup/finalization.
	deployLabels := maps.Clone(selectorLabels)
	maps.Copy(deployLabels, infraLabels)

	var tunnelResources *corev1.ResourceRequirements
	if params != nil && params.Spec.Tunnel != nil {
		tunnelResources = params.Spec.Tunnel.Resources
	}

	const portName = "http"
	tunnelArgs := []string{
		"tunnel",
		"--" + proxy.FlagNamespace, gw.Namespace,
		"--" + proxy.FlagConfigMapName, apiv1.GatewayResourceName(gw),
	}
	if params != nil && params.Spec.Tunnel != nil && params.Spec.Tunnel.Health != nil && params.Spec.Tunnel.Health.URL != "" {
		tunnelArgs = append(tunnelArgs, "--"+proxy.FlagHealthURL, params.Spec.Tunnel.Health.URL)
	}
	hasHealthURL := params != nil && params.Spec.Tunnel != nil && params.Spec.Tunnel.Health != nil && params.Spec.Tunnel.Health.URL != ""
	tunnelContainer := accorev1.Container().
		WithName("tunnel").
		WithImage(r.TunnelImage).
		WithArgs(tunnelArgs...).
		WithEnv(accorev1.EnvVar().
			WithName(cloudflare.TunnelTokenSecretKey).
			WithValueFrom(accorev1.EnvVarSource().
				WithSecretKeyRef(accorev1.SecretKeySelector().
					WithName(apiv1.GatewayResourceName(gw)).
					WithKey(cloudflare.TunnelTokenSecretKey),
				),
			),
		).
		WithPorts(accorev1.ContainerPort().
			WithName(portName).
			WithContainerPort(8080).
			WithProtocol(corev1.ProtocolTCP),
		).
		WithResources(resolveResources(tunnelResources)).
		WithLivenessProbe(accorev1.Probe().
			WithHTTPGet(accorev1.HTTPGetAction().
				WithPath("/healthz").
				WithPort(intstr.FromString(portName)),
			),
		).
		WithReadinessProbe(accorev1.Probe().
			WithHTTPGet(accorev1.HTTPGetAction().
				WithPath("/readyz").
				WithPort(intstr.FromString(portName)),
			),
		)
	if hasHealthURL {
		// When a health URL is configured, the /healthz liveness probe
		// goes through Cloudflare (tunnel + DNS), which takes time to
		// become reachable during startup. A startup probe gives the pod
		// time to initialize before the liveness probe starts.
		tunnelContainer = tunnelContainer.WithStartupProbe(accorev1.Probe().
			WithHTTPGet(accorev1.HTTPGetAction().
				WithPath("/healthz").
				WithPort(intstr.FromString(portName)),
			).
			WithPeriodSeconds(2).
			WithFailureThreshold(60),
		)
	}

	deploy := acappsv1.Deployment(deploymentName, gw.Namespace).
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
			WithReplicas(1).
			WithStrategy(acappsv1.DeploymentStrategy().
				WithType(appsv1.RollingUpdateDeploymentStrategyType).
				WithRollingUpdate(acappsv1.RollingUpdateDeployment().
					WithMaxSurge(intstr.FromInt32(1)).
					WithMaxUnavailable(intstr.FromInt32(0)),
				),
			).
			WithSelector(acmetav1.LabelSelector().
				WithMatchLabels(selectorLabels),
			).
			WithTemplate(accorev1.PodTemplateSpec().
				WithLabels(templateLabels).
				WithAnnotations(templateAnnotations).
				WithSpec(accorev1.PodSpec().
					WithServiceAccountName(apiv1.GatewayResourceName(gw)).
					WithContainers(tunnelContainer),
				),
			),
		)

	return deploy
}

// reconcileTunnelRBAC creates/updates the ServiceAccount, Role, and RoleBinding
// granting the tunnel read access to the route ConfigMap. Returns change messages.
func (r *GatewayReconciler) reconcileTunnelRBAC(ctx context.Context, gw *gatewayv1.Gateway) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string

	resourceName := apiv1.GatewayResourceName(gw)
	labels := tunnelRBACLabels(gw)

	ownerRef := metav1.OwnerReference{
		APIVersion:         gatewayv1.GroupVersion.String(),
		Kind:               apiv1.KindGateway,
		Name:               gw.Name,
		UID:                gw.UID,
		BlockOwnerDeletion: new(true),
		Controller:         new(true),
	}

	// ServiceAccount
	saTyped := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       apiv1.KindServiceAccount,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            resourceName,
			Namespace:       gw.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	sa, err := toUnstructured(saTyped)
	if err != nil {
		return nil, fmt.Errorf("converting tunnel ServiceAccount to unstructured: %w", err)
	}

	ssaEntry, err := r.ResourceManager.Apply(ctx, sa, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying tunnel ServiceAccount %s: %w", resourceName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("tunnel ServiceAccount %s %s", resourceName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled tunnel ServiceAccount", "serviceaccount", resourceName, "action", ssaEntry.Action)

	// Role
	roleTyped := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       apiv1.KindRole,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            resourceName,
			Namespace:       gw.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"configmaps"},
				ResourceNames: []string{resourceName},
				Verbs:         []string{"get", "list", "watch"},
			},
		},
	}
	role, err := toUnstructured(roleTyped)
	if err != nil {
		return nil, fmt.Errorf("converting tunnel Role to unstructured: %w", err)
	}

	ssaEntry, err = r.ResourceManager.Apply(ctx, role, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying tunnel Role %s: %w", resourceName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("tunnel Role %s %s", resourceName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled tunnel Role", "role", resourceName, "action", ssaEntry.Action)

	// RoleBinding
	rbTyped := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       apiv1.KindRoleBinding,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            resourceName,
			Namespace:       gw.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.SchemeGroupVersion.Group,
			Kind:     apiv1.KindRole,
			Name:     resourceName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      apiv1.KindServiceAccount,
			Name:      resourceName,
			Namespace: gw.Namespace,
		}},
	}
	rb, err := toUnstructured(rbTyped)
	if err != nil {
		return nil, fmt.Errorf("converting tunnel RoleBinding to unstructured: %w", err)
	}

	ssaEntry, err = r.ResourceManager.Apply(ctx, rb, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying tunnel RoleBinding %s: %w", resourceName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("tunnel RoleBinding %s %s", resourceName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled tunnel RoleBinding", "rolebinding", resourceName, "action", ssaEntry.Action)

	return changes, nil
}

// tunnelRBACLabels returns the standard labels for tunnel RBAC resources.
func tunnelRBACLabels(gw *gatewayv1.Gateway) map[string]string {
	lbls := apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentTunnel)
	maps.Copy(lbls, infrastructureLabels(gw.Spec.Infrastructure))
	return lbls
}

// removeOwnerReferencesFromTunnelRBACResources removes the Gateway's owner references
// from tunnel ServiceAccounts, Roles, and RoleBindings so they survive garbage
// collection when the Gateway is deleted with reconciliation disabled.
func (r *GatewayReconciler) removeOwnerReferencesFromTunnelRBACResources(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object
	matchLabels := client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentTunnel))

	// ServiceAccounts
	var saList corev1.ServiceAccountList
	if err := r.List(ctx, &saList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing tunnel ServiceAccounts: %w", err)
	}
	for i := range saList.Items {
		sa := &saList.Items[i]
		saKey := client.ObjectKeyFromObject(sa)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, saKey, sa); err != nil {
				return client.IgnoreNotFound(err)
			}
			saPatch := client.MergeFromWithOptions(sa.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(sa, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, sa, saPatch); err != nil {
				return fmt.Errorf("patching ServiceAccount %s: %w", sa.Name, err)
			}
			sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(apiv1.KindServiceAccount))
			removed = append(removed, sa)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from tunnel ServiceAccount", "serviceaccount", saKey)
	}

	// Roles
	var roleList rbacv1.RoleList
	if err := r.List(ctx, &roleList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing tunnel Roles: %w", err)
	}
	for i := range roleList.Items {
		role := &roleList.Items[i]
		roleKey := client.ObjectKeyFromObject(role)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, roleKey, role); err != nil {
				return client.IgnoreNotFound(err)
			}
			rolePatch := client.MergeFromWithOptions(role.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(role, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, role, rolePatch); err != nil {
				return fmt.Errorf("patching Role %s: %w", role.Name, err)
			}
			role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind(apiv1.KindRole))
			removed = append(removed, role)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from tunnel Role", "role", roleKey)
	}

	// RoleBindings
	var rbList rbacv1.RoleBindingList
	if err := r.List(ctx, &rbList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing tunnel RoleBindings: %w", err)
	}
	for i := range rbList.Items {
		rb := &rbList.Items[i]
		rbKey := client.ObjectKeyFromObject(rb)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, rbKey, rb); err != nil {
				return client.IgnoreNotFound(err)
			}
			rbPatch := client.MergeFromWithOptions(rb.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(rb, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, rb, rbPatch); err != nil {
				return fmt.Errorf("patching RoleBinding %s: %w", rb.Name, err)
			}
			rb.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind(apiv1.KindRoleBinding))
			removed = append(removed, rb)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from tunnel RoleBinding", "rolebinding", rbKey)
	}

	return removed, nil
}

// resolveResources returns the resource requirements apply configuration
// for a container. When explicit resources are provided, those are used
// (via JSON round-trip). Otherwise hardcoded defaults are returned.
func resolveResources(resources *corev1.ResourceRequirements) *accorev1.ResourceRequirementsApplyConfiguration {
	if resources != nil {
		data, err := json.Marshal(resources)
		if err == nil {
			var ac accorev1.ResourceRequirementsApplyConfiguration
			if err := json.Unmarshal(data, &ac); err == nil {
				return &ac
			}
		}
	}
	return accorev1.ResourceRequirements().
		WithRequests(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}).
		WithLimits(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		})
}
