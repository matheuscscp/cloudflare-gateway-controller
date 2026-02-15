// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import "time"

const (
	// ValueEnabled is the annotation value used to enable a feature.
	ValueEnabled = "enabled"
	// ValueDisabled is the annotation value used to disable a feature.
	ValueDisabled = "disabled"
)

const prefix = Group + "/"

const (
	// FinalizerGateway is the finalizer added to Gateway resources to ensure
	// the Cloudflare tunnel is deleted before the Gateway is removed.
	FinalizerGateway = prefix + "finalizer"

	// AnnotationTunnelID is the annotation key used to store the Cloudflare
	// tunnel ID on a Gateway resource.
	AnnotationTunnelID = prefix + "tunnel-id"

	// AnnotationReconcile is the annotation key used to enable or disable
	// reconciliation. Set to "disabled" to pause reconciliation.
	AnnotationReconcile = prefix + "reconcile"

	// AnnotationReconcileEvery is the annotation key used to override
	// the default reconciliation interval. The value must be a valid
	// Go duration string (e.g. "5m", "1h").
	AnnotationReconcileEvery = prefix + "reconcileEvery"

	// AnnotationReconcileTimeout is the annotation key used to override
	// the default timeout for health checks after applying resources.
	// The value must be a valid Go duration string (e.g. "5m", "1h").
	AnnotationReconcileTimeout = prefix + "reconcileTimeout"
)

const (
	// DefaultReconcileInterval is the default interval between periodic
	// reconciliations for drift correction.
	DefaultReconcileInterval = 10 * time.Minute

	// DefaultReconcileTimeout is the default timeout for health checks
	// after applying resources.
	DefaultReconcileTimeout = 5 * time.Minute
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

// ReconcileTimeout returns the reconciliation timeout for an object
// based on its annotations.
func ReconcileTimeout(annotations map[string]string) time.Duration {
	val, ok := annotations[AnnotationReconcileTimeout]
	if !ok {
		return DefaultReconcileTimeout
	}
	timeout, err := time.ParseDuration(val)
	if err != nil {
		return DefaultReconcileTimeout
	}
	return timeout
}
