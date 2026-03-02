// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	"fmt"
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var cfClusterName string

// SetClusterName sets the cluster name for deterministic resource naming.
// Must be called before any naming functions are used.
func SetClusterName(name string) { cfClusterName = name }

// ClusterName returns the configured cluster name.
func ClusterName() string { return cfClusterName }

const (
	// GroupCore is the Kubernetes core API group name used in Gateway API
	// references (e.g. parametersRef.group).
	GroupCore = "core"
)

// APIVersion constants.
const (
	APIVersionCore        = "v1"
	APIVersionApps        = "apps/v1"
	APIVersionRBAC        = "rbac.authorization.k8s.io/v1"
	APIVersionAutoscaling = "autoscaling.k8s.io/v1"
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
	KindConfigMap                   = "ConfigMap"
	KindServiceAccount              = "ServiceAccount"
	KindRole                        = "Role"
	KindRoleBinding                 = "RoleBinding"
	KindVerticalPodAutoscaler       = "VerticalPodAutoscaler"
)

// CRD names.
const (
	CRDGatewayClass = "gatewayclasses.gateway.networking.k8s.io"
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

// Label values.
const (
	// LabelAppNameCloudflared is the app.kubernetes.io/name value used on all
	// resources managed by a Gateway managed by this controller.
	LabelAppNameCloudflared = "cloudflared"

	// LabelAppComponentRoutes is the app.kubernetes.io/component value for the route ConfigMap.
	LabelAppComponentRoutes = "routes"

	// LabelAppComponentSidecar is the app.kubernetes.io/component value for sidecar RBAC resources.
	LabelAppComponentSidecar = "sidecar"
)

// Annotation values.
const (
	AnnotationReconcileEnabled  = "enabled"
	AnnotationReconcileDisabled = "disabled"
)

// Reconciliation defaults.
const (
	// DefaultReconcileInterval is the default interval between periodic
	// reconciliations for drift correction.
	DefaultReconcileInterval = 10 * time.Minute
)

// GatewayResourceName returns the name used for shared Kubernetes resources
// (Secret, ConfigMap, ServiceAccount, Role, RoleBinding) managed by a Gateway.
func GatewayResourceName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("gateway-%s", gw.Name)
}

// GatewayReplicaName returns the Deployment name for a specific replica
// of a Gateway: gateway-{gw.Name}-{replicaName}.
func GatewayReplicaName(gw *gatewayv1.Gateway, replicaName string) string {
	return fmt.Sprintf("%s-%s", GatewayResourceName(gw), replicaName)
}

// GatewayResourceLabels returns the standard app.kubernetes.io labels shared by
// all resources managed for a Gateway. If component is provided, the first value
// is set as the app.kubernetes.io/component label.
func GatewayResourceLabels(gwName string, component ...string) map[string]string {
	lbls := map[string]string{
		"app.kubernetes.io/name":       LabelAppNameCloudflared,
		"app.kubernetes.io/managed-by": ShortControllerName,
		"app.kubernetes.io/instance":   gwName,
	}
	if len(component) > 0 {
		lbls["app.kubernetes.io/component"] = component[0]
	}
	return lbls
}

// TunnelName returns the deterministic Cloudflare tunnel name for a Gateway.
func TunnelName(gw *gatewayv1.Gateway) string {
	return fmt.Sprintf("%s/clusters/%s/namespaces/%s/gateways/%s", Group, cfClusterName, gw.Namespace, gw.Name)
}

// ReconcileInterval returns the reconciliation interval for an object
// based on its annotations. Returns 0 if reconciliation is disabled.
// Returns an error if the reconcileEvery annotation has an invalid duration.
func ReconcileInterval(annotations map[string]string) (time.Duration, error) {
	if annotations[AnnotationReconcile] == AnnotationReconcileDisabled {
		return 0, nil
	}
	val, ok := annotations[AnnotationReconcileEvery]
	if !ok {
		return DefaultReconcileInterval, nil
	}
	interval, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("annotation %s has invalid duration %q: %w", AnnotationReconcileEvery, val, err)
	}
	return interval, nil
}
