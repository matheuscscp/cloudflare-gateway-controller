// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	corev1 "k8s.io/api/core/v1"
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

	// DNS configures DNS CNAME record management. When absent, DNS
	// management is enabled for all hostnames in attached HTTPRoutes
	// (each hostname's zone is resolved dynamically via the Cloudflare
	// API). Set dns.zones to restrict management to specific zones, or
	// set dns.zones to an empty list to disable DNS management entirely.
	// +optional
	DNS *DNSConfig `json:"dns,omitempty"`

	// Tunnel configures Cloudflare tunnel settings.
	// +optional
	Tunnel *TunnelConfig `json:"tunnel,omitempty"`
}

// SecretRef is a reference to a Secret in the same namespace.
type SecretRef struct {
	// Name of the Secret.
	Name string `json:"name"`
}

// DNSConfig configures DNS CNAME record management.
//
// By default (when this field is absent from the spec), DNS management is
// enabled for ALL hostnames in attached HTTPRoutes — each hostname's zone
// is resolved dynamically via the Cloudflare API.
//
// Set this field with a non-empty zones list to restrict DNS management to
// hostnames that are single-level subdomains of the listed zones.
//
// Set this field with an empty zones list (dns.zones: []) to explicitly
// disable DNS management.
type DNSConfig struct {
	// Zones restricts DNS CNAME management to hostnames matching these
	// zones. When empty, DNS management is disabled. When this field is
	// absent (i.e. the entire dns object is omitted), DNS management is
	// enabled for all hostnames.
	// +kubebuilder:validation:MaxItems=128
	Zones []DNSZoneConfig `json:"zones"`
}

// DNSZoneConfig identifies a Cloudflare DNS zone.
type DNSZoneConfig struct {
	// Name is the DNS zone name (e.g. "example.com").
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// TokenConfig configures tunnel token management.
//
// Token rotation is enabled by default (interval: 24h) even when this
// field is absent from the spec or when the rotation sub-field is absent.
// To disable automatic rotation, set rotation.enabled to false explicitly.
//
// On-demand rotation can always be triggered via
// "cfgwctl rotate gateway token", regardless of this configuration.
type TokenConfig struct {
	// Rotation configures automatic token rotation. When absent, rotation
	// is enabled with the default interval (24h). Set rotation.enabled to
	// false to disable automatic rotation.
	// +optional
	Rotation *TokenRotationConfig `json:"rotation,omitempty"`
}

// TokenRotationConfig configures automatic tunnel token rotation.
//
// When this struct is present (even if all fields are omitted), automatic
// rotation is enabled unless explicitly disabled via enabled: false.
type TokenRotationConfig struct {
	// Enabled controls whether automatic token rotation is active.
	// When absent, defaults to true (rotation is on). Set to false to
	// disable automatic rotation. Even when disabled, on-demand rotation
	// via "cfgwctl rotate gateway token" still works.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Interval is the minimum duration between automatic token rotations.
	// Defaults to 24h when absent or when set to zero or a negative value.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// TunnelConfig configures Cloudflare tunnel settings.
//
// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas.all(r, self.replicas.exists_one(s, s.name == r.name))",message="replica names must be unique"
type TunnelConfig struct {
	// Token configures tunnel token management.
	// +optional
	Token *TokenConfig `json:"token,omitempty"`

	// Resources configures compute resource requirements for the tunnel container.
	// When absent, the controller uses defaults (requests: 50m CPU, 64Mi
	// memory; limits: 500m CPU, 256Mi memory). When set, replaces defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Autoscaling configures vertical autoscaling for the tunnel container.
	// +optional
	Autoscaling *AutoscalingConfig `json:"autoscaling,omitempty"`

	// Patches are RFC 6902 JSON Patch operations applied to the
	// tunnel Deployment. Patches run after the controller builds the
	// base Deployment but before replica placement fields (affinity,
	// zone, nodeSelector) are applied on top. Replica placement always
	// takes priority over user patches.
	//
	// Patch errors are terminal — the controller stops retrying until the
	// CloudflareGatewayParameters resource is updated.
	// +optional
	Patches []JSONPatchOperation `json:"patches,omitempty"`

	// Replicas configures multiple replicas of the tunnel pods for high
	// availability. There are no guarantees about how requests coming
	// from Cloudflare will be distributed among replicas, but the
	// reverse proxy can improve load balancing when proxying to backend
	// Services by leveraging either kube-proxy or a service mesh.
	//
	// When absent (nil), the controller creates a single replica named "primary".
	// When explicitly empty ([]), no Deployments are created (scale to zero).
	// When non-empty, each entry creates a separate Deployment.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Replicas []ReplicaConfig `json:"replicas"`
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

// AutoscalingConfig configures vertical autoscaling for a container.
type AutoscalingConfig struct {
	// Enabled controls whether VPA recommendations are produced and
	// applied for this container. Defaults to false.
	Enabled bool `json:"enabled"`

	// MinAllowed specifies the minimum amount of resources that will be
	// recommended for the container. The default is no minimum.
	// +optional
	MinAllowed corev1.ResourceList `json:"minAllowed,omitempty"`

	// MaxAllowed specifies the maximum amount of resources that will be
	// recommended for the container. The default is no maximum.
	// +optional
	MaxAllowed corev1.ResourceList `json:"maxAllowed,omitempty"`

	// ControlledResources specifies which resource types are subject to
	// autoscaling. Allowed values are "cpu" and "memory". The default
	// is both cpu and memory.
	// +optional
	ControlledResources []corev1.ResourceName `json:"controlledResources,omitempty"`

	// ControlledValues specifies which resource values are subject to
	// autoscaling. Allowed values are "RequestsAndLimits" and
	// "RequestsOnly". The default is "RequestsAndLimits".
	// +optional
	// +kubebuilder:validation:Enum=RequestsAndLimits;RequestsOnly
	ControlledValues *string `json:"controlledValues,omitempty"`
}

// ReplicaConfig configures a single replica of the tunnel pods.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.zone) && has(self.affinity))",message="zone and affinity are mutually exclusive"
type ReplicaConfig struct {
	// Name identifies this replica.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// Zone is shorthand for topology.kubernetes.io/zone node affinity.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Zone string `json:"zone,omitempty"`

	// NodeSelector is a map of label key-value pairs for node selection.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity is a full Kubernetes affinity spec for pod placement.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// +kubebuilder:object:root=true

// CloudflareGatewayParametersList contains a list of CloudflareGatewayParameters.
type CloudflareGatewayParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareGatewayParameters `json:"items"`
}
