// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"

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

// detectLBMode determines the load balancer topology mode from the parameters.
func detectLBMode(params *apiv1.CloudflareGatewayParameters) lbMode {
	if params == nil || params.Spec.LoadBalancer == nil {
		return lbModeNone
	}
	switch params.Spec.LoadBalancer.Topology {
	case apiv1.LoadBalancerTopologyHighAvailability:
		return lbModePerAZ
	case apiv1.LoadBalancerTopologyTrafficSplitting:
		return lbModePerBackendRef
	default:
		return lbModeNone
	}
}

// computeDesiredTunnels produces the list of desired tunnel entries based on
// the LB mode, parameters, and valid routes. In simple mode a single entry
// is returned; in per-AZ mode one entry per AZ; in per-backendRef mode one
// entry per unique Service (times per AZ if AZs are configured).
func computeDesiredTunnels(gw *gatewayv1.Gateway, mode lbMode, params *apiv1.CloudflareGatewayParameters, validRoutes []*gatewayv1.HTTPRoute) []tunnelEntry {
	switch mode {
	case lbModePerAZ:
		azs := params.Spec.Tunnels.AvailabilityZones
		entries := make([]tunnelEntry, 0, len(azs))
		for _, az := range azs {
			entries = append(entries, tunnelEntry{
				tunnelName:     apiv1.TunnelNameForAZ(gw, az.Name),
				deploymentName: apiv1.CloudflaredDeploymentNameForAZ(gw, az.Name),
				secretName:     apiv1.TunnelTokenSecretNameForAZ(gw, az.Name),
				azName:         az.Name,
			})
		}
		return entries
	case lbModePerBackendRef:
		services := collectUniqueServices(validRoutes)
		hasAZs := params.Spec.Tunnels != nil && len(params.Spec.Tunnels.AvailabilityZones) > 0
		var entries []tunnelEntry
		for _, svc := range services {
			if hasAZs {
				for _, az := range params.Spec.Tunnels.AvailabilityZones {
					entries = append(entries, tunnelEntry{
						tunnelName:       apiv1.TunnelNameForServiceAZ(gw, svc.Namespace, svc.Name, az.Name),
						deploymentName:   apiv1.CloudflaredDeploymentNameForServiceAZ(gw, svc.Name, az.Name),
						secretName:       apiv1.TunnelTokenSecretNameForServiceAZ(gw, svc.Name, az.Name),
						azName:           az.Name,
						serviceNamespace: svc.Namespace,
						serviceName:      svc.Name,
					})
				}
			} else {
				entries = append(entries, tunnelEntry{
					tunnelName:       apiv1.TunnelNameForService(gw, svc.Namespace, svc.Name),
					deploymentName:   apiv1.CloudflaredDeploymentNameForService(gw, svc.Name),
					secretName:       apiv1.TunnelTokenSecretNameForService(gw, svc.Name),
					serviceNamespace: svc.Namespace,
					serviceName:      svc.Name,
				})
			}
		}
		return entries
	default:
		return []tunnelEntry{{
			tunnelName:     apiv1.TunnelName(gw),
			deploymentName: apiv1.CloudflaredDeploymentName(gw),
			secretName:     apiv1.TunnelTokenSecretName(gw),
		}}
	}
}

// collectUniqueServices extracts unique namespace-qualified Service references
// from the backendRefs of valid HTTPRoutes, sorted deterministically. The
// namespace defaults to the route's namespace when not specified on the ref.
func collectUniqueServices(routes []*gatewayv1.HTTPRoute) []types.NamespacedName {
	seen := make(map[types.NamespacedName]struct{})
	var services []types.NamespacedName
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			for _, ref := range rule.BackendRefs {
				ns := route.Namespace
				if ref.Namespace != nil {
					ns = string(*ref.Namespace)
				}
				key := types.NamespacedName{Namespace: ns, Name: string(ref.Name)}
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					services = append(services, key)
				}
			}
		}
	}
	slices.SortFunc(services, func(a, b types.NamespacedName) int {
		if a.Namespace != b.Namespace {
			if a.Namespace < b.Namespace {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return services
}

// findAZConfig looks up the AvailabilityZone config by name from the parameters.
func findAZConfig(params *apiv1.CloudflareGatewayParameters, azName string) *apiv1.AvailabilityZone {
	if params == nil || params.Spec.Tunnels == nil {
		return nil
	}
	for i := range params.Spec.Tunnels.AvailabilityZones {
		if params.Spec.Tunnels.AvailabilityZones[i].Name == azName {
			return &params.Spec.Tunnels.AvailabilityZones[i]
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
// (tunnels, LB, DNS). Conditions are patched separately in patchGatewayStatus.
// Returns the CGS pointer (which may be newly created) so the caller can pass
// it to patchGatewayStatus for condition mirroring.
func (r *GatewayReconciler) reconcileCGS(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	cgs *apiv1.CloudflareGatewayStatus,
	entries []tunnelEntry,
	zoneName string,
	lbState lbReconcileState,
) (*apiv1.CloudflareGatewayStatus, error) {
	// Build desired status detail.
	desired := apiv1.CloudflareGatewayStatusDetail{}

	// Tunnels.
	for _, e := range entries {
		desired.Tunnels = append(desired.Tunnels, apiv1.TunnelStatus{
			Name:             e.tunnelName,
			ID:               e.tunnelID,
			DeploymentName:   e.deploymentName,
			SecretName:       e.secretName,
			AZName:           e.azName,
			ServiceNamespace: e.serviceNamespace,
			ServiceName:      e.serviceName,
		})
	}

	// DNS zone info.
	if zoneName != "" {
		desired.DNS = &apiv1.DNSStatus{
			ZoneName: zoneName,
			ZoneID:   lbState.zoneID,
		}
	}

	// LB state.
	if lbState.monitorID != "" {
		desired.LoadBalancer = &apiv1.LoadBalancerStatus{
			MonitorID:   lbState.monitorID,
			MonitorName: lbState.monitorName,
			Pools:       lbState.pools,
			Hostnames:   lbState.hostnames,
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

// cgsLBHostnames extracts the LB hostnames from a CloudflareGatewayStatus.
// Returns nil if the CGS is nil or has no LB state.
func cgsLBHostnames(cgs *apiv1.CloudflareGatewayStatus) []string {
	if cgs == nil || cgs.Status.LoadBalancer == nil {
		return nil
	}
	return cgs.Status.LoadBalancer.Hostnames
}

// cgsLBPools extracts the LB pool statuses from a CloudflareGatewayStatus.
// Returns nil if the CGS is nil or has no LB state.
func cgsLBPools(cgs *apiv1.CloudflareGatewayStatus) []apiv1.PoolStatus {
	if cgs == nil || cgs.Status.LoadBalancer == nil {
		return nil
	}
	return cgs.Status.LoadBalancer.Pools
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
