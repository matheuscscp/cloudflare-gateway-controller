// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	"fmt"
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Gateway CRD names.
const (
	CRDGatewayClass = "gatewayclasses.gateway.networking.k8s.io"
)

// Kind constants.
const (
	KindCustomResourceDefinition = "CustomResourceDefinition"
	KindGatewayClass             = "GatewayClass"
	KindGateway                  = "Gateway"
	KindHTTPRoute                = "HTTPRoute"
	KindReferenceGrant           = "ReferenceGrant"
	KindNamespace                = "Namespace"
	KindSecret                   = "Secret"
	KindService                  = "Service"
	KindDeployment               = "Deployment"
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
)

// FinalizerGatewayClass returns the finalizer added to a GatewayClass for a
// specific Gateway, ensuring the GatewayClass is not deleted while the Gateway
// references it.
func FinalizerGatewayClass(gw *gatewayv1.Gateway) string {
	return string(gatewayv1.GatewayClassFinalizerGatewaysExist) + "/" + gw.Name + "." + gw.Namespace
}

// Annotations.
const (
	// AnnotationBundleVersion is the annotation used to track the version of the Gateway API
	// that the controller is compatible with.
	AnnotationBundleVersion = "gateway.networking.k8s.io/bundle-version"

	// AnnotationReconcile enables or disables reconciliation.
	// Set to "disabled" to pause reconciliation.
	AnnotationReconcile = prefix + "reconcile"

	// AnnotationReconcileEvery overrides the default reconciliation interval.
	// The value must be a valid Go duration string (e.g. "5m", "1h").
	AnnotationReconcileEvery = prefix + "reconcileEvery"

	// AnnotationZoneName opts a Gateway into DNS CNAME management for the given zone.
	// When set, the Gateway controller manages all DNS CNAME records for hostnames
	// from attached HTTPRoutes, pointing to the tunnel's cfargotunnel.com target.
	AnnotationZoneName = prefixGateway + "zoneName"

	// AnnotationDeploymentPatches customizes the cloudflared Deployment via
	// RFC 6902 JSON Patch operations written in YAML. The value is a YAML-encoded
	// array of patch operations applied after the base Deployment is built.
	// Example:
	//   - op: add
	//     path: /spec/template/spec/tolerations
	//     value:
	//       - key: "example.com/special-node"
	//         operator: "Exists"
	AnnotationDeploymentPatches = prefixGateway + "deploymentPatches"
)

// Reconciliation defaults.
const (
	// DefaultReconcileInterval is the default interval between periodic
	// reconciliations for drift correction.
	DefaultReconcileInterval = 10 * time.Minute
)

// TunnelName returns the deterministic Cloudflare tunnel name for a Gateway.
// The name is globally unique because it includes the Gateway's UID.
func TunnelName(gw *gatewayv1.Gateway) string {
	return "gateway-" + string(gw.UID)
}

// CloudflaredDeploymentName returns the name of the cloudflared Deployment for a Gateway.
func CloudflaredDeploymentName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("cloudflared-%s", gw.Name)
}

// TunnelTokenSecretName returns the name of the tunnel token Secret for a Gateway.
func TunnelTokenSecretName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("cloudflared-token-%s", gw.Name)
}

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
