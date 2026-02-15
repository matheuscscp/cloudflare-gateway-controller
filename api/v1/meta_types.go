// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Gateway API kind constants (not exported by the upstream package).
const (
	KindGateway   = "Gateway"
	KindHTTPRoute = "HTTPRoute"
	KindSecret    = "Secret"
	KindService   = "Service"
)

// Annotation values.
const (
	ValueEnabled  = "enabled"
	ValueDisabled = "disabled"
)

// Finalizers.
const (
	// Finalizer is the finalizer added to resources managed by this controller
	// to ensure cleanup is performed before the resource is removed.
	Finalizer = prefix + "finalizer"

	// FinalizerGatewayClass is the finalizer added to GatewayClass resources
	// to ensure the GatewayClass is not deleted while Gateways reference it.
	FinalizerGatewayClass = gatewayv1.GatewayClassFinalizerGatewaysExist + "/finalizer"
)

// Annotations.
const (
	// AnnotationReconcile enables or disables reconciliation.
	// Set to "disabled" to pause reconciliation.
	AnnotationReconcile = prefix + "reconcile"

	// AnnotationReconcileEvery overrides the default reconciliation interval.
	// The value must be a valid Go duration string (e.g. "5m", "1h").
	AnnotationReconcileEvery = prefix + "reconcileEvery"

	// AnnotationTunnelName overrides the default Cloudflare tunnel name.
	// When absent, the Gateway UID is used.
	AnnotationTunnelName = prefixGateway + "tunnelName"

	// AnnotationReplicas sets the number of cloudflared replicas.
	// When absent on create, defaults to 1.
	// When absent on update, the current value is preserved.
	AnnotationReplicas = prefixGateway + "replicas"
)

// Reconciliation defaults.
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
