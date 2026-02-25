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

// TunnelNameForAZ returns the Cloudflare tunnel name for a Gateway in a specific AZ.
func TunnelNameForAZ(gw *gatewayv1.Gateway, azName string) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name, azName)
}

// TunnelNameForService returns the Cloudflare tunnel name for a Gateway
// dedicated to a specific Service (traffic splitting mode, no AZs).
func TunnelNameForService(gw *gatewayv1.Gateway, svcNamespace, svcName string) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name, svcNamespace, svcName)
}

// TunnelNameForServiceAZ returns the Cloudflare tunnel name for a Gateway
// dedicated to a specific Service in a specific AZ (traffic splitting + AZs).
func TunnelNameForServiceAZ(gw *gatewayv1.Gateway, svcNamespace, svcName, azName string) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name, svcNamespace, svcName, azName)
}

// CloudflaredDeploymentName returns the name of the cloudflared Deployment for a Gateway.
func CloudflaredDeploymentName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("cloudflared-%s", gw.Name)
}

// CloudflaredDeploymentNameForAZ returns the name of the cloudflared Deployment
// for a Gateway in a specific AZ.
func CloudflaredDeploymentNameForAZ(gw *gatewayv1.Gateway, azName string) string {
	return fmt.Sprintf("cloudflared-%s-%s", gw.Name, azName)
}

// CloudflaredDeploymentNameForService returns the name of the cloudflared
// Deployment for a Gateway dedicated to a specific Service (no AZs).
func CloudflaredDeploymentNameForService(gw *gatewayv1.Gateway, serviceName string) string {
	return fmt.Sprintf("cloudflared-%s-%s", gw.Name, serviceName)
}

// CloudflaredDeploymentNameForServiceAZ returns the name of the cloudflared
// Deployment for a Gateway dedicated to a specific Service in a specific AZ.
func CloudflaredDeploymentNameForServiceAZ(gw *gatewayv1.Gateway, serviceName, azName string) string {
	return fmt.Sprintf("cloudflared-%s-%s-%s", gw.Name, serviceName, azName)
}

// TunnelTokenSecretName returns the name of the tunnel token Secret for a Gateway.
func TunnelTokenSecretName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("cloudflared-token-%s", gw.Name)
}

// TunnelTokenSecretNameForAZ returns the name of the tunnel token Secret
// for a Gateway in a specific AZ.
func TunnelTokenSecretNameForAZ(gw *gatewayv1.Gateway, azName string) string {
	return fmt.Sprintf("cloudflared-token-%s-%s", gw.Name, azName)
}

// TunnelTokenSecretNameForService returns the name of the tunnel token Secret
// for a Gateway dedicated to a specific Service (no AZs).
func TunnelTokenSecretNameForService(gw *gatewayv1.Gateway, serviceName string) string {
	return fmt.Sprintf("cloudflared-token-%s-%s", gw.Name, serviceName)
}

// TunnelTokenSecretNameForServiceAZ returns the name of the tunnel token Secret
// for a Gateway dedicated to a specific Service in a specific AZ.
func TunnelTokenSecretNameForServiceAZ(gw *gatewayv1.Gateway, serviceName, azName string) string {
	return fmt.Sprintf("cloudflared-token-%s-%s-%s", gw.Name, serviceName, azName)
}

// MonitorName returns the Cloudflare LB monitor name for a Gateway.
func MonitorName(gw *gatewayv1.Gateway) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name, "monitor")
}

// PoolNameForAZ returns the Cloudflare LB pool name for a Gateway AZ
// (geographic steering mode).
func PoolNameForAZ(gw *gatewayv1.Gateway, azName string) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name, azName)
}

// PoolNameForService returns the Cloudflare LB pool name for a Gateway Service
// (traffic splitting mode).
func PoolNameForService(gw *gatewayv1.Gateway, svcNamespace, svcName string) string {
	return ResourceName(cfClusterName, gw.Namespace, gw.Name, svcNamespace, svcName)
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
