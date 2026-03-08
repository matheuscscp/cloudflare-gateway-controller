// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=cgs
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Tunnel Name",type=string,JSONPath=`.status.tunnel.name`,priority=1
// +kubebuilder:printcolumn:name="Tunnel ID",type=string,JSONPath=`.status.tunnel.id`
// +kubebuilder:printcolumn:name="DNS",type=string,JSONPath=`.status.conditions[?(@.type=="DNSManagement")].reason`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

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
	// DNSManagement, Ready) so users can inspect them on the CGS directly.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Tunnel holds the Cloudflare tunnel managed for this Gateway.
	// +optional
	Tunnel *TunnelStatus `json:"tunnel,omitempty"`

	// Inventory lists all Kubernetes objects managed by this Gateway.
	// +optional
	Inventory []ResourceRef `json:"inventory,omitempty"`

	// LastHandledReconcileAt is the value of the reconcileRequestedAt annotation
	// at the time the last reconciliation was handled.
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`

	// LastHandledTokenRotateAt is the value of the rotateTokenRequestedAt annotation
	// at the time the last token rotation request was handled.
	// +optional
	LastHandledTokenRotateAt string `json:"lastHandledTokenRotateAt,omitempty"`

	// LastTokenRotatedAt is the timestamp of the last successful token rotation
	// (either scheduled or on-demand).
	// +optional
	LastTokenRotatedAt string `json:"lastTokenRotatedAt,omitempty"`

	// CurrentTokenHash is the truncated SHA-256 hex digest of the current
	// tunnel token, as seen by the controller during reconciliation.
	// +optional
	CurrentTokenHash string `json:"currentTokenHash,omitempty"`
}

// TunnelStatus records the state of the Cloudflare tunnel managed for
// a Gateway.
type TunnelStatus struct {
	// Name is the Cloudflare tunnel name.
	Name string `json:"name"`

	// ID is the Cloudflare tunnel UUID.
	ID string `json:"id"`
}

// ResourceRef identifies a Kubernetes object managed by a Gateway.
type ResourceRef struct {
	// APIVersion is the API group and version of the resource (e.g. "v1", "apps/v1").
	APIVersion string `json:"apiVersion"`

	// Kind is the resource kind (e.g. "Deployment", "Secret").
	Kind string `json:"kind"`

	// Name is the resource name (same namespace as the Gateway).
	Name string `json:"name"`
}

// +kubebuilder:object:root=true

// CloudflareGatewayStatusList contains a list of CloudflareGatewayStatus.
type CloudflareGatewayStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareGatewayStatus `json:"items"`
}
