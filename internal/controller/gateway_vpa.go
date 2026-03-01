// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	"github.com/fluxcd/pkg/ssa"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

// autoscalingEnabled returns true when at least one container has
// autoscaling.enabled set to true. Nil-safe walk through the params.
func autoscalingEnabled(params *apiv1.CloudflareGatewayParameters) bool {
	if params == nil || params.Spec.Tunnel == nil {
		return false
	}
	t := params.Spec.Tunnel
	if t.Cloudflared != nil && t.Cloudflared.Autoscaling != nil && t.Cloudflared.Autoscaling.Enabled {
		return true
	}
	if t.Sidecar != nil && t.Sidecar.Autoscaling != nil && t.Sidecar.Autoscaling.Enabled {
		return true
	}
	return false
}

// containerAutoscalingEnabled returns true when a specific container's
// autoscaling is enabled. Nil-safe.
func containerAutoscalingEnabled(cc *apiv1.ContainerConfig) bool {
	return cc != nil && cc.Autoscaling != nil && cc.Autoscaling.Enabled
}

// reconcileVPAs creates or updates a VerticalPodAutoscaler for each replica
// Deployment. Returns change messages.
func (r *GatewayReconciler) reconcileVPAs(ctx context.Context, gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters, replicas []apiv1.ReplicaConfig) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string

	// Build container policies. Containers with autoscaling enabled get
	// mode Auto; all others are opted out via the "*" wildcard set to Off
	// (unlisted containers default to Auto in the VPA API).
	containerPolicies := []vpav1.ContainerResourcePolicy{{
		ContainerName: vpav1.DefaultContainerResourcePolicy,
		Mode:          new(vpav1.ContainerScalingModeOff),
	}}
	if params != nil && params.Spec.Tunnel != nil {
		t := params.Spec.Tunnel
		if t.Cloudflared != nil && containerAutoscalingEnabled(&t.Cloudflared.ContainerConfig) {
			containerPolicies = append(containerPolicies,
				buildContainerPolicy("cloudflared", t.Cloudflared.Autoscaling))
		}
		if t.Sidecar != nil && containerAutoscalingEnabled(&t.Sidecar.ContainerConfig) {
			containerPolicies = append(containerPolicies,
				buildContainerPolicy("sidecar", t.Sidecar.Autoscaling))
		}
	}

	updateMode := vpav1.UpdateModeInPlaceOrRecreate

	for i := range replicas {
		name := apiv1.GatewayReplicaName(gw, replicas[i].Name)

		vpa := &vpav1.VerticalPodAutoscaler{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "autoscaling.k8s.io/v1",
				Kind:       apiv1.KindVerticalPodAutoscaler,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: gw.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":       "cloudflared",
					"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
					"app.kubernetes.io/instance":   gw.Name,
					"app.kubernetes.io/component":  replicas[i].Name,
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         gatewayv1.GroupVersion.String(),
					Kind:               apiv1.KindGateway,
					Name:               gw.Name,
					UID:                gw.UID,
					BlockOwnerDeletion: new(true),
					Controller:         new(true),
				}},
			},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{
					APIVersion: apiv1.APIVersionApps,
					Kind:       apiv1.KindDeployment,
					Name:       name,
				},
				UpdatePolicy: &vpav1.PodUpdatePolicy{
					UpdateMode: &updateMode,
				},
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: containerPolicies,
				},
			},
		}

		obj, err := toUnstructured(vpa)
		if err != nil {
			return changes, fmt.Errorf("converting VPA %s to unstructured: %w", name, err)
		}

		ssaEntry, err := r.ResourceManager.Apply(ctx, obj, ssaApplyOptions)
		if err != nil {
			return changes, fmt.Errorf("applying VPA %s: %w", name, err)
		}
		if ssaEntry.Action != ssa.UnchangedAction {
			changes = append(changes, fmt.Sprintf("VPA %s %s", name, ssaEntry.Action))
		}
		l.V(1).Info("Reconciled VPA", "vpa", name, "action", ssaEntry.Action)
	}

	return changes, nil
}

// cleanupStaleVPAs deletes VPAs that are no longer in the desired set.
// This handles: all autoscaling disabled (empty desired set), replica removal,
// and autoscaling being turned off. VPA CRD may not be installed — list/delete
// errors are non-fatal (logged, collected as errs).
func (r *GatewayReconciler) cleanupStaleVPAs(ctx context.Context, gw *gatewayv1.Gateway, desiredNames map[string]struct{}) ([]string, []string) {
	l := log.FromContext(ctx)

	var vpaList vpav1.VerticalPodAutoscalerList
	if err := r.List(ctx, &vpaList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":       "cloudflared",
			"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
			"app.kubernetes.io/instance":   gw.Name,
		},
	); err != nil {
		// CRD may not be installed — non-fatal.
		l.V(1).Info("Failed to list VPAs for cleanup (CRD may not be installed)", "error", err)
		return nil, nil
	}

	var changes []string
	var errs []string
	for i := range vpaList.Items {
		vpa := &vpaList.Items[i]
		if _, ok := desiredNames[vpa.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, vpa); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Sprintf("failed to delete stale VPA %s: %v", vpa.Name, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted stale VPA %s", vpa.Name))
		l.V(1).Info("Deleted stale VPA", "vpa", vpa.Name)
	}

	return changes, errs
}

// removeOwnerReferencesFromVPAs removes the Gateway's owner references from
// VPAs so they survive garbage collection when the Gateway is deleted with
// reconciliation disabled. VPA CRD may not be installed — errors are non-fatal.
func (r *GatewayReconciler) removeOwnerReferencesFromVPAs(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object

	var vpaList vpav1.VerticalPodAutoscalerList
	if err := r.List(ctx, &vpaList,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":       "cloudflared",
			"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
			"app.kubernetes.io/instance":   gw.Name,
		},
	); err != nil {
		// CRD may not be installed — non-fatal.
		l.V(1).Info("Failed to list VPAs for owner reference removal (CRD may not be installed)", "error", err)
		return nil, nil
	}

	for i := range vpaList.Items {
		vpa := &vpaList.Items[i]
		vpaKey := types.NamespacedName{Namespace: vpa.Namespace, Name: vpa.Name}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, vpaKey, vpa); err != nil {
				return client.IgnoreNotFound(err)
			}
			vpaPatch := client.MergeFromWithOptions(vpa.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(vpa, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, vpa, vpaPatch); err != nil {
				return fmt.Errorf("patching VPA %s: %w", vpa.Name, err)
			}
			removed = append(removed, vpa)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from VPA", "vpa", vpaKey)
	}

	return removed, nil
}

// buildContainerPolicy builds a VPA ContainerResourcePolicy from an
// AutoscalingConfig, mapping minAllowed, maxAllowed, controlledResources,
// and controlledValues.
func buildContainerPolicy(name string, ac *apiv1.AutoscalingConfig) vpav1.ContainerResourcePolicy {
	cp := vpav1.ContainerResourcePolicy{
		ContainerName: name,
		Mode:          new(vpav1.ContainerScalingModeAuto),
		MinAllowed:    ac.MinAllowed,
		MaxAllowed:    ac.MaxAllowed,
	}
	if len(ac.ControlledResources) > 0 {
		resources := make([]corev1.ResourceName, len(ac.ControlledResources))
		copy(resources, ac.ControlledResources)
		cp.ControlledResources = &resources
	}
	if ac.ControlledValues != nil {
		cv := vpav1.ContainerControlledValues(*ac.ControlledValues)
		cp.ControlledValues = &cv
	}
	return cp
}

// toUnstructured converts a typed runtime.Object to *unstructured.Unstructured
// for use with the SSA ResourceManager.
func toUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: data}, nil
}
