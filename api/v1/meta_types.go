// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// ValueEnabled is the annotation value used to enable a feature.
	ValueEnabled = "enabled"
	// ValueDisabled is the annotation value used to disable a feature.
	ValueDisabled = "disabled"
)

const prefix = Group + "/"
const prefixGateway = "gateway." + prefix

const (
	// FinalizerGateway is the finalizer added to Gateway resources to ensure
	// the Cloudflare tunnel is deleted before the Gateway is removed.
	FinalizerGateway = prefix + "finalizer"

	// FinalizerGatewayClass is the finalizer added to GatewayClass resources
	// to ensure the GatewayClass is not deleted while Gateways reference it.
	FinalizerGatewayClass = gatewayv1.GatewayClassFinalizerGatewaysExist + "/finalizer"

	// AnnotationReconcile is the annotation key used to enable or disable
	// reconciliation. Set to "disabled" to pause reconciliation.
	AnnotationReconcile = prefix + "reconcile"

	// AnnotationReconcileEvery is the annotation key used to override
	// the default reconciliation interval. The value must be a valid
	// Go duration string (e.g. "5m", "1h").
	AnnotationReconcileEvery = prefix + "reconcileEvery"

	// AnnotationTunnelName is the annotation key used to override the
	// default Cloudflare tunnel name. When absent, the Gateway UID is used.
	AnnotationTunnelName = prefixGateway + "tunnelName"

	// AnnotationReplicas is the annotation key used to set the number
	// of cloudflared replicas. When absent on create, defaults to 1.
	// When absent on update, the current value is preserved.
	AnnotationReplicas = prefixGateway + "replicas"
)

const (
	// DefaultReconcileInterval is the default interval between periodic
	// reconciliations for drift correction.
	DefaultReconcileInterval = 10 * time.Minute
)

// ReconcileInterval returns the reconciliation interval for an object
// based on its annotations. Returns 0 if reconciliation is disabled.
func ReconcileInterval(annotations map[string]string) time.Duration {
	if annotations[AnnotationReconcile] == ValueDisabled {
		return 0
	}
	val, ok := annotations[AnnotationReconcileEvery]
	if !ok {
		return DefaultReconcileInterval
	}
	interval, err := time.ParseDuration(val)
	if err != nil {
		return DefaultReconcileInterval
	}
	return interval
}
