// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

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

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// ensureTunnels creates or looks up all desired tunnels, filling in the
// tunnelID field on each entry. Returns a list of change messages for created tunnels.
func (r *GatewayReconciler) ensureTunnels(ctx context.Context, tc cloudflare.Client, entries []tunnelEntry) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string
	for i := range entries {
		e := &entries[i]
		tunnelID, err := tc.GetTunnelIDByName(ctx, e.tunnelName)
		if err != nil {
			return nil, fmt.Errorf("looking up tunnel %q: %w", e.tunnelName, err)
		}
		if tunnelID != "" {
			e.tunnelID = tunnelID
			continue
		}
		tunnelID, err = tc.CreateTunnel(ctx, e.tunnelName)
		if err != nil {
			if !cloudflare.IsConflict(err) {
				return nil, fmt.Errorf("creating tunnel %q: %w", e.tunnelName, err)
			}
			tunnelID, err = tc.GetTunnelIDByName(ctx, e.tunnelName)
			if err != nil {
				return nil, fmt.Errorf("looking up tunnel %q after conflict: %w", e.tunnelName, err)
			}
		}
		e.tunnelID = tunnelID
		changes = append(changes, fmt.Sprintf("created tunnel %s", e.tunnelName))
		l.V(1).Info("Created tunnel", "tunnelName", e.tunnelName, "tunnelID", tunnelID)
	}
	return changes, nil
}

// reconcileTunnelTokenSecrets reconciles a tunnel token Secret for each entry,
// setting the Gateway as the controller owner reference. Returns change messages.
func (r *GatewayReconciler) reconcileTunnelTokenSecrets(ctx context.Context, gw *gatewayv1.Gateway, tc cloudflare.Client, entries []tunnelEntry) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string
	for i := range entries {
		e := &entries[i]
		tunnelToken, err := tc.GetTunnelToken(ctx, e.tunnelID)
		if err != nil {
			return nil, fmt.Errorf("getting tunnel token for %q: %w", e.tunnelName, err)
		}
		secret := buildTunnelTokenSecretForEntry(gw, e, tunnelToken)
		if err := controllerutil.SetControllerReference(gw, secret, r.Scheme()); err != nil {
			return nil, fmt.Errorf("setting owner reference on secret %q: %w", e.secretName, err)
		}
		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			desired := buildTunnelTokenSecretForEntry(gw, e, tunnelToken)
			secret.Data = desired.Data
			secret.Labels = desired.Labels
			secret.Annotations = desired.Annotations
			return controllerutil.SetControllerReference(gw, secret, r.Scheme())
		})
		if err != nil {
			return nil, fmt.Errorf("creating/updating tunnel token secret %q: %w", e.secretName, err)
		}
		if result != controllerutil.OperationResultNone {
			changes = append(changes, fmt.Sprintf("tunnel token Secret %s %s", e.secretName, result))
		}
		l.V(1).Info("Reconciled tunnel token Secret", "secret", e.secretName, "result", result)
	}
	return changes, nil
}

// applyCloudflaredDeployments builds and applies a cloudflared Deployment for
// each tunnel entry via Server-Side Apply. Returns change messages.
func (r *GatewayReconciler) applyCloudflaredDeployments(ctx context.Context, gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters, entries []tunnelEntry) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string
	for i := range entries {
		e := &entries[i]
		deployObj, err := r.buildCloudflaredDeployment(gw, params, e)
		if err != nil {
			return nil, fmt.Errorf("building cloudflared deployment %q: %w", e.deploymentName, err)
		}
		ssaEntry, err := r.ResourceManager.Apply(ctx, deployObj, ssaApplyOptions)
		if err != nil {
			return nil, fmt.Errorf("applying cloudflared deployment %q: %w", e.deploymentName, err)
		}
		if string(ssaEntry.Action) != string(ssa.UnchangedAction) {
			changes = append(changes, fmt.Sprintf("cloudflared Deployment %s %s", e.deploymentName, ssaEntry.Action))
		}
		l.V(1).Info("Reconciled cloudflared Deployment", "deployment", e.deploymentName, "action", ssaEntry.Action)
	}
	return changes, nil
}

// checkAllDeploymentsReadiness fetches all cloudflared Deployments for the
// tunnel entries and inspects their status to determine the Gateway's
// Programmed and Ready state. All Deployments must be available for the
// Gateway to be considered Programmed.
func (r *GatewayReconciler) checkAllDeploymentsReadiness(ctx context.Context, gw *gatewayv1.Gateway, entries []tunnelEntry) gatewayReadiness {
	l := log.FromContext(ctx)
	allAvailable := true
	anyDeadlineExceeded := false
	var notReadyNames []string

	for i := range entries {
		e := &entries[i]
		var deploy appsv1.Deployment
		deployAvailable := false
		deployDeadlineExceeded := false
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: gw.Namespace,
			Name:      e.deploymentName,
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
			l.V(1).Info("Failed to get Deployment, treating as unavailable", "deployment", e.deploymentName, "error", err)
		}
		if deployDeadlineExceeded {
			anyDeadlineExceeded = true
			notReadyNames = append(notReadyNames, e.deploymentName)
		} else if !deployAvailable {
			allAvailable = false
			notReadyNames = append(notReadyNames, e.deploymentName)
		}
	}

	readiness := gatewayReadiness{
		readyStatus:      metav1.ConditionUnknown,
		readyReason:      apiv1.ReasonProgressingWithRetry,
		programmedStatus: metav1.ConditionFalse,
		programmedReason: string(gatewayv1.GatewayReasonPending),
		programmedMsg:    "Waiting for cloudflared deployment to become ready",
	}
	if anyDeadlineExceeded {
		readiness.readyStatus = metav1.ConditionFalse
		readiness.readyReason = apiv1.ReasonReconciliationFailed
		readiness.readyMsg = fmt.Sprintf("cloudflared deployment(s) exceeded progress deadline: %s", strings.Join(notReadyNames, ", "))
	} else if allAvailable {
		readiness.programmedStatus = metav1.ConditionTrue
		readiness.programmedReason = string(gatewayv1.GatewayReasonProgrammed)
		readiness.programmedMsg = "Gateway is programmed"
		readiness.readyStatus = metav1.ConditionTrue
		readiness.readyReason = apiv1.ReasonReconciliationSucceeded
		readiness.readyMsg = "Gateway is ready"
	} else {
		readiness.readyReason = apiv1.ReasonProgressing
		readiness.readyMsg = fmt.Sprintf("Waiting for cloudflared deployment(s) to become ready: %s", strings.Join(notReadyNames, ", "))
	}
	return readiness
}

// removeOwnerReferences removes the Gateway's owner references from all
// managed cloudflared Deployments and tunnel token Secrets so they survive
// garbage collection when the Gateway is deleted with reconciliation disabled.
// Returns the list of resources that were modified.
func (r *GatewayReconciler) removeOwnerReferences(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object
	matchLabels := client.MatchingLabels{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
		"app.kubernetes.io/instance":   gw.Name,
	}

	// Remove owner references from all cloudflared Deployments.
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

// cleanupStaleTunnelResources deletes Deployments, Secrets, and Cloudflare tunnels
// that are no longer part of the desired tunnel entries. This handles AZ
// addition/removal, service changes, and mode switches.
func (r *GatewayReconciler) cleanupStaleTunnelResources(ctx context.Context, gw *gatewayv1.Gateway, entries []tunnelEntry) ([]string, []string) {
	l := log.FromContext(ctx)

	// Build set of desired Deployment names.
	desired := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		desired[e.deploymentName] = struct{}{}
	}

	// List all cloudflared Deployments owned by this Gateway.
	var deployList appsv1.DeploymentList
	if err := r.List(ctx, &deployList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":       "cloudflared",
			"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
			"app.kubernetes.io/instance":   gw.Name,
		},
	); err != nil {
		return nil, []string{fmt.Sprintf("failed to list cloudflared Deployments for cleanup: %v", err)}
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
		l.V(1).Info("Deleted stale cloudflared Deployment", "deployment", deploy.Name)
	}

	// Delete corresponding stale Secrets (by convention, Secrets owned by the
	// Gateway with the cloudflared label pattern). We use owner references
	// to find them via GC, but also clean up explicitly.
	var secretList corev1.SecretList
	if err := r.List(ctx, &secretList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":       "cloudflared",
			"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
			"app.kubernetes.io/instance":   gw.Name,
		},
	); err != nil {
		errs = append(errs, fmt.Sprintf("failed to list tunnel token Secrets for cleanup: %v", err))
		return changes, errs
	}

	desiredSecrets := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		desiredSecrets[e.secretName] = struct{}{}
	}
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

func buildTunnelTokenSecretForEntry(gw *gatewayv1.Gateway, e *tunnelEntry, tunnelToken string) *corev1.Secret {
	lbls := infrastructureLabels(gw.Spec.Infrastructure)
	if lbls == nil {
		lbls = make(map[string]string)
	}
	lbls["app.kubernetes.io/name"] = "cloudflared"
	lbls["app.kubernetes.io/managed-by"] = apiv1.ShortControllerName
	lbls["app.kubernetes.io/instance"] = gw.Name
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        e.secretName,
			Namespace:   gw.Namespace,
			Labels:      lbls,
			Annotations: infrastructureAnnotations(gw.Spec.Infrastructure),
		},
		Data: map[string][]byte{
			"TUNNEL_TOKEN": []byte(tunnelToken),
		},
	}
}

// buildCloudflaredDeployment builds the desired cloudflared Deployment as an
// unstructured object suitable for server-side apply via the SSA manager.
// If the CloudflareGatewayParameters has deployment patches, the RFC 6902
// JSON Patch operations are applied to the base Deployment. AZ placement is
// applied last so it always takes effect.
func (r *GatewayReconciler) buildCloudflaredDeployment(gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters, e *tunnelEntry) (*unstructured.Unstructured, error) {
	apply := r.buildCloudflaredDeploymentApply(gw, e)
	data, err := json.Marshal(apply)
	if err != nil {
		return nil, fmt.Errorf("marshaling deployment: %w", err)
	}

	// Apply RFC 6902 JSON Patch operations from CloudflareGatewayParameters if present.
	// These errors are terminal because the CRD is a watched object and
	// retrying won't fix invalid user input.
	if params != nil && params.Spec.Tunnel != nil && params.Spec.Tunnel.Deployment != nil && len(params.Spec.Tunnel.Deployment.Patches) > 0 {
		patchJSON, err := json.Marshal(params.Spec.Tunnel.Deployment.Patches)
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

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("unmarshaling deployment: %w", err)
	}
	obj.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(apiv1.KindDeployment))
	return obj, nil
}

// buildCloudflaredDeploymentApply builds the apply configuration for the
// cloudflared Deployment, including selector labels, infrastructure
// labels/annotations, owner reference, and the cloudflared container spec.
func (r *GatewayReconciler) buildCloudflaredDeploymentApply(gw *gatewayv1.Gateway, e *tunnelEntry) *acappsv1.DeploymentApplyConfiguration {
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
		"app.kubernetes.io/instance":   gw.Name,
	}
	templateLabels := maps.Clone(selectorLabels)

	infraLabels := infrastructureLabels(gw.Spec.Infrastructure)
	deployAnnotations := infrastructureAnnotations(gw.Spec.Infrastructure)
	maps.Copy(templateLabels, infraLabels)
	templateAnnotations := infrastructureAnnotations(gw.Spec.Infrastructure)

	// Deployment metadata gets both infrastructure labels and selector labels
	// so the controller can list Deployments by label for cleanup/finalization.
	deployLabels := maps.Clone(selectorLabels)
	maps.Copy(deployLabels, infraLabels)

	deploy := acappsv1.Deployment(e.deploymentName, gw.Namespace).
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
									WithName(e.secretName).
									WithKey("TUNNEL_TOKEN"),
								),
							),
						).
						WithResources(accorev1.ResourceRequirements().
							WithRequests(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							}).
							WithLimits(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							}),
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
