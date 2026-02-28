// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// readParameters resolves the CloudflareGatewayParameters from the Gateway's
// infrastructure.parametersRef. Returns nil if no CloudflareGatewayParameters
// is referenced (i.e. only a Secret is used or no parametersRef is set).
func readParameters(ctx context.Context, r client.Reader, gw *gatewayv1.Gateway) (*apiv1.CloudflareGatewayParameters, error) {
	if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
		return nil, nil
	}
	ref := gw.Spec.Infrastructure.ParametersRef
	if string(ref.Kind) != apiv1.KindCloudflareGatewayParameters || ref.Group != gatewayv1.Group(apiv1.Group) {
		return nil, nil
	}
	var params apiv1.CloudflareGatewayParameters
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: gw.Namespace,
		Name:      ref.Name,
	}, &params); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, reconcile.TerminalError(fmt.Errorf("CloudflareGatewayParameters %s/%s not found", gw.Namespace, ref.Name))
		}
		return nil, fmt.Errorf("getting CloudflareGatewayParameters %s/%s: %w", gw.Namespace, ref.Name, err)
	}
	return &params, nil
}

// readCredentials reads the Cloudflare API credentials from:
//  1. CloudflareGatewayParameters spec.secretRef (if params has one), or
//  2. Gateway infrastructure.parametersRef (if it directly references a Secret), or
//  3. GatewayClass parametersRef (must be a Secret).
//
// If infrastructure.parametersRef is set but references an unsupported kind,
// an error is returned. Cross-namespace references from GatewayClass are
// validated against ReferenceGrants.
func readCredentials(ctx context.Context, r client.Reader, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway, params *apiv1.CloudflareGatewayParameters) (cloudflare.ClientConfig, error) {
	var secretNamespace, secretName string

	switch {
	case params != nil && params.Spec.SecretRef != nil:
		// 1. CloudflareGatewayParameters with secretRef.
		secretNamespace = params.Namespace
		secretName = params.Spec.SecretRef.Name
	case gw.Spec.Infrastructure != nil && gw.Spec.Infrastructure.ParametersRef != nil:
		// infrastructure.parametersRef is set — must be a Secret or a CGP (handled above).
		ref := gw.Spec.Infrastructure.ParametersRef
		if string(ref.Kind) == apiv1.KindSecret && (ref.Group == "" || ref.Group == apiv1.GroupCore) {
			// 2. Gateway infrastructure.parametersRef pointing to a Secret.
			secretNamespace = gw.Namespace
			secretName = ref.Name
		} else if string(ref.Kind) == apiv1.KindCloudflareGatewayParameters && ref.Group == gatewayv1.Group(apiv1.Group) {
			// CGP without secretRef — fall through to GatewayClass.
			return readCredentialsFromGatewayClass(ctx, r, gc, gw)
		} else {
			return cloudflare.ClientConfig{}, reconcile.TerminalError(fmt.Errorf("infrastructure parametersRef must reference a core/v1 Secret or a CloudflareGatewayParameters"))
		}
	default:
		// 3. GatewayClass parametersRef (Secret).
		return readCredentialsFromGatewayClass(ctx, r, gc, gw)
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

// readCredentialsFromGatewayClass reads the Cloudflare API credentials from the
// GatewayClass parametersRef, which must be a Secret. Cross-namespace references
// are validated against ReferenceGrants.
func readCredentialsFromGatewayClass(ctx context.Context, r client.Reader, gc *gatewayv1.GatewayClass, gw *gatewayv1.Gateway) (cloudflare.ClientConfig, error) {
	if gc.Spec.ParametersRef == nil {
		return cloudflare.ClientConfig{}, reconcile.TerminalError(fmt.Errorf("gatewayclass %q has no parametersRef", gc.Name))
	}
	ref := gc.Spec.ParametersRef
	if string(ref.Kind) != apiv1.KindSecret || (ref.Group != "" && ref.Group != apiv1.GroupCore && ref.Group != gatewayv1.Group("")) {
		return cloudflare.ClientConfig{}, reconcile.TerminalError(fmt.Errorf("parametersRef must reference a core/v1 Secret"))
	}
	if ref.Namespace == nil {
		return cloudflare.ClientConfig{}, reconcile.TerminalError(fmt.Errorf("parametersRef must specify a namespace"))
	}
	secretNamespace := string(*ref.Namespace)
	secretName := ref.Name

	if granted, err := secretReferenceGranted(ctx, r, gw.Namespace, secretNamespace, secretName); err != nil {
		return cloudflare.ClientConfig{}, fmt.Errorf("checking ReferenceGrant: %w", err)
	} else if !granted {
		return cloudflare.ClientConfig{}, reconcile.TerminalError(fmt.Errorf("cross-namespace reference to Secret %s/%s not allowed by any ReferenceGrant", secretNamespace, secretName))
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
		if err := r.Patch(ctx, gc, gcPatch); err != nil {
			return err
		}
		log.FromContext(ctx).V(1).Info("Added finalizer to GatewayClass", "gatewayclass", gc.Name)
		return nil
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
			if err := r.Patch(ctx, gc, gcPatch); err != nil {
				return err
			}
			log.FromContext(ctx).V(1).Info("Removed finalizer from GatewayClass", "gatewayclass", gc.Name)
			return nil
		}); err != nil {
			return fmt.Errorf("removing finalizer from GatewayClass %s: %w", gc.Name, err)
		}
	}
	return nil
}

// getCGS fetches the CloudflareGatewayStatus for a Gateway. Returns nil (not
// an error) when the CGS does not exist yet.
func (r *GatewayReconciler) getCGS(ctx context.Context, gw *gatewayv1.Gateway) (*apiv1.CloudflareGatewayStatus, error) {
	var cgs apiv1.CloudflareGatewayStatus
	if err := r.Get(ctx, client.ObjectKeyFromObject(gw), &cgs); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &cgs, nil
}

// reconcileCGS creates or updates the CloudflareGatewayStatus resource state
// (tunnels, DNS). Conditions are patched separately in patchGatewayStatus.
// Returns the CGS pointer (which may be newly created) so the caller can pass
// it to patchGatewayStatus for condition mirroring.
func (r *GatewayReconciler) reconcileCGS(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	cgs *apiv1.CloudflareGatewayStatus,
	entries []tunnelEntry,
	zoneName string,
	zoneID string,
) (*apiv1.CloudflareGatewayStatus, error) {
	// Build desired status detail.
	desired := apiv1.CloudflareGatewayStatusDetail{}

	// Tunnels.
	for _, e := range entries {
		desired.Tunnels = append(desired.Tunnels, apiv1.TunnelStatus{
			Name:           e.tunnelName,
			ID:             e.tunnelID,
			DeploymentName: e.deploymentName,
			SecretName:     e.secretName,
		})
	}

	// DNS zone info.
	if zoneName != "" {
		desired.DNS = &apiv1.DNSStatus{
			ZoneName: zoneName,
			ZoneID:   zoneID,
		}
	}

	if cgs == nil || !cgs.DeletionTimestamp.IsZero() {
		if cgs != nil {
			// CGS is being deleted (e.g. manually by the user). Remove the
			// finalizer so it can be garbage-collected, then create a new one.
			cgsPatch := client.MergeFrom(cgs.DeepCopy())
			controllerutil.RemoveFinalizer(cgs, apiv1.Finalizer)
			if err := r.Patch(ctx, cgs, cgsPatch); err != nil {
				return nil, fmt.Errorf("removing finalizer from terminating CloudflareGatewayStatus: %w", err)
			}
			log.FromContext(ctx).V(1).Info("Removed finalizer from terminating CloudflareGatewayStatus")
		}

		// Create the CGS with an owner reference so it's garbage-collected
		// when the Gateway is deleted, and a finalizer to prevent accidental
		// deletion while the Gateway still exists.
		cgs = &apiv1.CloudflareGatewayStatus{}
		cgs.Name = gw.Name
		cgs.Namespace = gw.Namespace
		cgs.Finalizers = []string{apiv1.Finalizer}
		cgs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: gatewayv1.GroupVersion.String(),
			Kind:       apiv1.KindGateway,
			Name:       gw.Name,
			UID:        gw.UID,
			Controller: new(true),
		}}
		if err := r.Create(ctx, cgs); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The old CGS hasn't been fully deleted yet. The next
				// reconciliation will handle it.
				return nil, nil
			}
			return nil, fmt.Errorf("creating CloudflareGatewayStatus: %w", err)
		}
		// Patch status subresource (Create doesn't set status).
		cgsPatch := client.MergeFrom(cgs.DeepCopy())
		cgs.Status = desired
		if err := r.Status().Patch(ctx, cgs, cgsPatch); err != nil {
			return cgs, fmt.Errorf("patching CloudflareGatewayStatus status after create: %w", err)
		}
		log.FromContext(ctx).V(1).Info("Created CloudflareGatewayStatus")
		return cgs, nil
	}

	// Update existing CGS status (preserve conditions, they're patched separately).
	// Only patch when resource state (tunnels, DNS, LB) actually changed.
	desired.Conditions = cgs.Status.Conditions
	if !reflect.DeepEqual(cgs.Status, desired) {
		cgsPatch := client.MergeFrom(cgs.DeepCopy())
		cgs.Status = desired
		if err := r.Status().Patch(ctx, cgs, cgsPatch); err != nil {
			return cgs, fmt.Errorf("patching CloudflareGatewayStatus: %w", err)
		}
	}
	return cgs, nil
}

// cgsTunnels extracts the tunnel statuses from a CloudflareGatewayStatus.
// Returns nil if the CGS is nil.
func cgsTunnels(cgs *apiv1.CloudflareGatewayStatus) []apiv1.TunnelStatus {
	if cgs == nil {
		return nil
	}
	return cgs.Status.Tunnels
}

// cgsZoneInfo extracts the DNS zone name and ID from a CloudflareGatewayStatus.
// Returns empty strings if the CGS is nil or has no DNS info.
func cgsZoneInfo(cgs *apiv1.CloudflareGatewayStatus) (string, string) {
	if cgs == nil || cgs.Status.DNS == nil {
		return "", ""
	}
	return cgs.Status.DNS.ZoneName, cgs.Status.DNS.ZoneID
}

// infrastructureLabels returns the labels from the Gateway's infrastructure spec.
func infrastructureLabels(infra *gatewayv1.GatewayInfrastructure) map[string]string {
	if infra == nil {
		return nil
	}
	lbls := make(map[string]string, len(infra.Labels))
	for k, v := range infra.Labels {
		lbls[string(k)] = string(v)
	}
	return lbls
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
