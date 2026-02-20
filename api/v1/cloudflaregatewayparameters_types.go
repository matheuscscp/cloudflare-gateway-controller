// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=cgp

// CloudflareGatewayParameters provides typed configuration for Gateways
// managed by this controller. It can be referenced via
// infrastructure.parametersRef from a Gateway.
type CloudflareGatewayParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CloudflareGatewayParametersSpec `json:"spec,omitempty"`
}

// CloudflareGatewayParametersSpec defines the desired configuration.
type CloudflareGatewayParametersSpec struct {
	// SecretRef references a Secret in the same namespace containing
	// CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID.
	// +optional
	SecretRef *SecretRef `json:"secretRef,omitempty"`

	// DNS configures DNS CNAME record management.
	// +optional
	DNS *DNSConfig `json:"dns,omitempty"`

	// Tunnels configures Cloudflare tunnel settings.
	// +optional
	Tunnels *TunnelsConfig `json:"tunnels,omitempty"`
}

// SecretRef is a reference to a Secret in the same namespace.
type SecretRef struct {
	// Name of the Secret.
	Name string `json:"name"`
}

// DNSConfig configures DNS CNAME record management.
type DNSConfig struct {
	// Zone configures the DNS zone for CNAME management.
	Zone DNSZoneConfig `json:"zone"`
}

// DNSZoneConfig identifies a Cloudflare DNS zone.
type DNSZoneConfig struct {
	// Name is the DNS zone name (e.g. "example.com").
	Name string `json:"name"`
}

// TunnelsConfig configures Cloudflare tunnel settings.
type TunnelsConfig struct {
	// Cloudflared configures the cloudflared Deployment.
	// +optional
	Cloudflared *CloudflaredConfig `json:"cloudflared,omitempty"`
}

// CloudflaredConfig configures the cloudflared Deployment.
type CloudflaredConfig struct {
	// Patches are RFC 6902 JSON Patch operations applied to the
	// cloudflared Deployment after it is built.
	// +optional
	Patches []JSONPatchOperation `json:"patches,omitempty"`
}

// JSONPatchOperation represents a single RFC 6902 JSON Patch operation.
type JSONPatchOperation struct {
	// Op is the patch operation.
	// +kubebuilder:validation:Enum=add;remove;replace;move;copy;test
	Op string `json:"op"`

	// Path is the JSON Pointer path for the operation.
	Path string `json:"path"`

	// From is the source path for move and copy operations.
	// +optional
	From string `json:"from,omitempty"`

	// Value is the value to use for add, replace, and test operations.
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`
}

// +kubebuilder:object:root=true

// CloudflareGatewayParametersList contains a list of CloudflareGatewayParameters.
type CloudflareGatewayParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareGatewayParameters `json:"items"`
}
