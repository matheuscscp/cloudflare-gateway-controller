// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var cfClusterName string

// SetClusterName sets the cluster name for deterministic resource naming.
// Must be called before any naming functions are used.
func SetClusterName(name string) { cfClusterName = name }

// ClusterName returns the configured cluster name.
func ClusterName() string { return cfClusterName }

// ResourceName returns a deterministic resource name by hashing the
// fully-qualified ownership fields with SHA256. The result is "gw-" +
// the full 64 hex-char SHA256 digest (67 chars total).
func ResourceName(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte("/"))
		}
		h.Write([]byte(p))
	}
	return fmt.Sprintf("gw-%x", h.Sum(nil))
}

// ResourceDescription returns a compact ownership description for Cloudflare
// resource metadata (Description on pools/monitors/LBs, Comment on DNS records).
// The format is kept short to fit within the 100-char DNS comment limit on free plans.
func ResourceDescription(gw *gatewayv1.Gateway, extra ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cfgw cluster:%s ns:%s gw:%s",
		cfClusterName, gw.Namespace, gw.Name)
	for _, e := range extra {
		b.WriteString(" ")
		b.WriteString(e)
	}
	return b.String()
}

// CRD names.
const (
	CRDGatewayClass = "gatewayclasses.gateway.networking.k8s.io"
)

// Kind constants.
const (
	KindCustomResourceDefinition    = "CustomResourceDefinition"
	KindCloudflareGatewayParameters = "CloudflareGatewayParameters"
	KindCloudflareGatewayStatus     = "CloudflareGatewayStatus"
	KindGatewayClass                = "GatewayClass"
	KindGateway                     = "Gateway"
	KindHTTPRoute                   = "HTTPRoute"
	KindReferenceGrant              = "ReferenceGrant"
	KindNamespace                   = "Namespace"
	KindSecret                      = "Secret"
	KindService                     = "Service"
	KindDeployment                  = "Deployment"
)

// GroupCore is the Kubernetes core API group name used in Gateway API
// references (e.g. parametersRef.group).
const GroupCore = "core"

// Annotation values.
const (
	ValueEnabled  = "enabled"
	ValueDisabled = "disabled"
)

// Finalizers.
const (
	// Finalizer is the finalizer added to resources managed by this controller
	// to ensure cleanup is performed before the resource is removed.
	Finalizer = Group + "/finalizer"
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
	AnnotationReconcile = Group + "/reconcile"

	// AnnotationReconcileEvery overrides the default reconciliation interval.
	// The value must be a valid Go duration string (e.g. "5m", "1h").
	AnnotationReconcileEvery = Group + "/reconcileEvery"
)

// Reconciliation defaults.
const (
	// DefaultReconcileInterval is the default interval between periodic
	// reconciliations for drift correction.
	DefaultReconcileInterval = 10 * time.Minute
)

// TunnelName returns the deterministic Cloudflare tunnel name for a Gateway.
func TunnelName(gw *gatewayv1.Gateway) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name)
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
