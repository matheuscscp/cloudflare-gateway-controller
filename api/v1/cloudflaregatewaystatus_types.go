// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=cgs
// +kubebuilder:subresource:status

// CloudflareGatewayStatus stores the observable Cloudflare resource state
// for a Gateway managed by this controller. The controller creates one CGS
// per managed Gateway (same name and namespace) and keeps it up to date
// during reconciliation. This state is used for safe cleanup when the
// CloudflareGatewayParameters reference changes or is deleted.
type CloudflareGatewayStatus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            CloudflareGatewayStatusDetail `json:"status,omitempty"`
}

// CloudflareGatewayStatusDetail holds the full observable state of Cloudflare
// resources managed for a Gateway.
type CloudflareGatewayStatusDetail struct {
	// Conditions report the current state of the Gateway's Cloudflare resources.
	// Condition types mirror the Gateway's own conditions (Accepted, Programmed,
	// Ready) so users can inspect them on the CGS directly.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Tunnels lists all Cloudflare tunnels managed for this Gateway.
	// +optional
	Tunnels []TunnelStatus `json:"tunnels,omitempty"`

	// LoadBalancer holds the current LB resource state.
	// Nil when no LB topology is configured.
	// +optional
	LoadBalancer *LoadBalancerStatus `json:"loadBalancer,omitempty"`

	// DNS holds the DNS zone info used for CNAME and LB management.
	// +optional
	DNS *DNSStatus `json:"dns,omitempty"`
}

// TunnelStatus records the state of a single Cloudflare tunnel and its
// associated Kubernetes resources.
type TunnelStatus struct {
	// Name is the Cloudflare tunnel name.
	Name string `json:"name"`

	// ID is the Cloudflare tunnel UUID.
	ID string `json:"id"`

	// DeploymentName is the name of the cloudflared Deployment.
	DeploymentName string `json:"deploymentName"`

	// SecretName is the name of the tunnel token Secret.
	SecretName string `json:"secretName"`

	// AZName is the availability zone name (empty if not per-AZ).
	// +optional
	AZName string `json:"azName,omitempty"`

	// ServiceNamespace is the backend service namespace (empty if not per-service).
	// +optional
	ServiceNamespace string `json:"serviceNamespace,omitempty"`

	// ServiceName is the backend service name (empty if not per-service).
	// +optional
	ServiceName string `json:"serviceName,omitempty"`
}

// LoadBalancerStatus records the state of Cloudflare Load Balancer resources.
type LoadBalancerStatus struct {
	// MonitorID is the Cloudflare monitor UUID.
	MonitorID string `json:"monitorId"`

	// MonitorName is the Cloudflare monitor name.
	MonitorName string `json:"monitorName"`

	// Pools lists the Cloudflare LB pools.
	// +optional
	Pools []PoolStatus `json:"pools,omitempty"`

	// Hostnames lists the load-balanced hostnames.
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`
}

// PoolStatus records the state of a single Cloudflare LB pool.
type PoolStatus struct {
	// Name is the Cloudflare pool name.
	Name string `json:"name"`

	// ID is the Cloudflare pool UUID.
	ID string `json:"id"`
}

// DNSStatus records the DNS zone used for CNAME and LB management.
type DNSStatus struct {
	// ZoneName is the DNS zone name (e.g. "example.com").
	ZoneName string `json:"zoneName"`

	// ZoneID is the Cloudflare zone UUID.
	ZoneID string `json:"zoneID"`
}

// +kubebuilder:object:root=true

// CloudflareGatewayStatusList contains a list of CloudflareGatewayStatus.
type CloudflareGatewayStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareGatewayStatus `json:"items"`
}
