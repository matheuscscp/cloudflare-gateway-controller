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

	// DNS holds the DNS zone info used for CNAME management.
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
}

// DNSStatus records the DNS zone used for CNAME management.
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
