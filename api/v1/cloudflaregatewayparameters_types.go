// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Load balancer topology values.
const (
	LoadBalancerTopologyHighAvailability = "HighAvailability"
	LoadBalancerTopologyTrafficSplitting = "TrafficSplitting"
)

// Steering policy CRD values (PascalCase, as written by users in the CRD).
const (
	SteeringPolicyOff                      = "Off"
	SteeringPolicyGeographic               = "Geographic"
	SteeringPolicyRandom                   = "Random"
	SteeringPolicyDynamicLatency           = "DynamicLatency"
	SteeringPolicyProximity                = "Proximity"
	SteeringPolicyLeastOutstandingRequests = "LeastOutstandingRequests"
	SteeringPolicyLeastConnections         = "LeastConnections"
)

// Steering policy Cloudflare API values.
const (
	CloudflareSteeringPolicyOff                      = "off"
	CloudflareSteeringPolicyGeo                      = "geo"
	CloudflareSteeringPolicyRandom                   = "random"
	CloudflareSteeringPolicyDynamicLatency           = "dynamic_latency"
	CloudflareSteeringPolicyProximity                = "proximity"
	CloudflareSteeringPolicyLeastOutstandingRequests = "least_outstanding_requests"
	CloudflareSteeringPolicyLeastConnections         = "least_connections"
)

// CloudflareSteeringPolicy maps a CRD steering policy value to its Cloudflare
// API equivalent. Unknown values default to "off".
func CloudflareSteeringPolicy(v string) string {
	switch v {
	case SteeringPolicyOff:
		return CloudflareSteeringPolicyOff
	case SteeringPolicyGeographic:
		return CloudflareSteeringPolicyGeo
	case SteeringPolicyRandom:
		return CloudflareSteeringPolicyRandom
	case SteeringPolicyDynamicLatency:
		return CloudflareSteeringPolicyDynamicLatency
	case SteeringPolicyProximity:
		return CloudflareSteeringPolicyProximity
	case SteeringPolicyLeastOutstandingRequests:
		return CloudflareSteeringPolicyLeastOutstandingRequests
	case SteeringPolicyLeastConnections:
		return CloudflareSteeringPolicyLeastConnections
	default:
		return CloudflareSteeringPolicyOff
	}
}

// Session affinity CRD values.
const (
	SessionAffinityNone     = "None"
	SessionAffinityCookie   = "Cookie"
	SessionAffinityIPCookie = "IPCookie"
	SessionAffinityHeader   = "Header"
)

// Session affinity Cloudflare API values.
const (
	CloudflareSessionAffinityNone     = "none"
	CloudflareSessionAffinityCookie   = "cookie"
	CloudflareSessionAffinityIPCookie = "ip_cookie"
	CloudflareSessionAffinityHeader   = "header"
)

// CloudflareSessionAffinity maps a CRD session affinity value to its Cloudflare
// API equivalent. Unknown values default to "none".
func CloudflareSessionAffinity(v string) string {
	switch v {
	case SessionAffinityNone:
		return CloudflareSessionAffinityNone
	case SessionAffinityCookie:
		return CloudflareSessionAffinityCookie
	case SessionAffinityIPCookie:
		return CloudflareSessionAffinityIPCookie
	case SessionAffinityHeader:
		return CloudflareSessionAffinityHeader
	default:
		return CloudflareSessionAffinityNone
	}
}

// Monitor type CRD values.
const (
	MonitorTypeHTTP  = "HTTP"
	MonitorTypeHTTPS = "HTTPS"
	MonitorTypeTCP   = "TCP"
)

// Monitor type Cloudflare API values.
const (
	CloudflareMonitorTypeHTTP  = "http"
	CloudflareMonitorTypeHTTPS = "https"
	CloudflareMonitorTypeTCP   = "tcp"
)

// CloudflareMonitorType maps a CRD monitor type value to its Cloudflare API
// equivalent. Unknown values default to "https".
func CloudflareMonitorType(v string) string {
	switch v {
	case MonitorTypeHTTP:
		return CloudflareMonitorTypeHTTP
	case MonitorTypeHTTPS:
		return CloudflareMonitorTypeHTTPS
	case MonitorTypeTCP:
		return CloudflareMonitorTypeTCP
	default:
		return CloudflareMonitorTypeHTTPS
	}
}

// Monitor defaults applied when CRD fields are omitted.
// Interval and Timeout default to 0, which causes the API to use the
// plan-specific defaults on the Cloudflare side.
const (
	DefaultMonitorType          = CloudflareMonitorTypeHTTPS
	DefaultMonitorPath          = "/ready"
	DefaultMonitorInterval      = int64(0)
	DefaultMonitorTimeout       = int64(0)
	DefaultMonitorExpectedCodes = "200"
)

// ResolvedMonitorConfig holds health monitor settings with defaults applied
// for any omitted CRD fields. Values are in Cloudflare API format.
type ResolvedMonitorConfig struct {
	Type          string
	Path          string
	Interval      int64
	Timeout       int64
	ExpectedCodes string
}

// ResolveMonitorConfig builds a ResolvedMonitorConfig from the CRD
// LoadBalancerConfig, applying defaults for omitted fields.
func ResolveMonitorConfig(lb *LoadBalancerConfig) ResolvedMonitorConfig {
	cfg := ResolvedMonitorConfig{
		Type:          DefaultMonitorType,
		Path:          DefaultMonitorPath,
		Interval:      DefaultMonitorInterval,
		Timeout:       DefaultMonitorTimeout,
		ExpectedCodes: DefaultMonitorExpectedCodes,
	}
	if lb == nil || lb.Monitor == nil {
		return cfg
	}
	m := lb.Monitor
	if m.Type != "" {
		cfg.Type = CloudflareMonitorType(m.Type)
	}
	if m.Path != "" {
		cfg.Path = m.Path
	}
	if m.Interval > 0 {
		cfg.Interval = m.Interval
	}
	if m.Timeout > 0 {
		cfg.Timeout = m.Timeout
	}
	return cfg
}

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
//
// +kubebuilder:validation:XValidation:rule="!(has(self.tunnels) && has(self.tunnels.availabilityZones) && size(self.tunnels.availabilityZones) > 0) || has(self.loadBalancer)",message="loadBalancer is required when tunnels.availabilityZones is set"
// +kubebuilder:validation:XValidation:rule="!has(self.loadBalancer) || self.loadBalancer.topology != 'HighAvailability' || (has(self.tunnels) && has(self.tunnels.availabilityZones) && size(self.tunnels.availabilityZones) > 0)",message="tunnels.availabilityZones is required when loadBalancer.topology is HighAvailability"
// +kubebuilder:validation:XValidation:rule="!has(self.loadBalancer) || has(self.dns)",message="dns is required when loadBalancer is set"
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

	// LoadBalancer configures Cloudflare Load Balancer settings.
	// +optional
	LoadBalancer *LoadBalancerConfig `json:"loadBalancer,omitempty"`
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

	// AvailabilityZones configures multi-AZ tunnel deployment.
	// Each entry creates a separate tunnel and cloudflared Deployment
	// placed in the specified zone/node selection.
	// +optional
	AvailabilityZones []AvailabilityZone `json:"availabilityZones,omitempty"`
}

// AvailabilityZone configures a single availability zone for tunnel deployment.
//
// +kubebuilder:validation:XValidation:rule="(has(self.zone) ? 1 : 0) + (has(self.nodeSelector) && size(self.nodeSelector) > 0 ? 1 : 0) + (has(self.affinity) ? 1 : 0) == 1",message="exactly one of zone, nodeSelector, or affinity must be set"
type AvailabilityZone struct {
	// Name identifies this AZ. Used in Deployment/tunnel/pool naming.
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

// LoadBalancerConfig configures Cloudflare Load Balancer settings.
type LoadBalancerConfig struct {
	// Topology selects the load balancer infrastructure shape.
	// "HighAvailability": one tunnel per AZ, each carrying full ingress
	//   rules. The LB steers traffic between zones while cloudflared handles
	//   path routing. Requires tunnels.availabilityZones.
	// "TrafficSplitting": one tunnel per Service backendRef (times per AZ if
	//   availabilityZones is set). Each tunnel routes all traffic to one
	//   service. The LB distributes traffic by HTTPRoute backendRef weights.
	// +kubebuilder:validation:Enum=HighAvailability;TrafficSplitting
	Topology string `json:"topology"`

	// SteeringPolicy controls how Cloudflare distributes traffic between pools.
	// Applies to both topologies but different values are typical for each:
	//
	// For HighAvailability (pools = AZs):
	//   "Geographic" — routes to closest pool by client region (recommended).
	//   "Proximity" — routes to lowest-latency pool based on health check RTT.
	//   "DynamicLatency" — learns latency over time and routes to fastest pool.
	//
	// For TrafficSplitting (pools = Services):
	//   "Random" — distributes traffic proportionally using pool weights
	//     derived from HTTPRoute backendRef weights (recommended).
	//
	// Other valid values: Off, LeastOutstandingRequests, LeastConnections.
	// +optional
	// +kubebuilder:validation:Enum=Off;Geographic;Random;DynamicLatency;Proximity;LeastOutstandingRequests;LeastConnections
	SteeringPolicy string `json:"steeringPolicy,omitempty"`

	// SessionAffinity controls session persistence across requests.
	// "None" — no session affinity (default if omitted).
	// "Cookie" — route subsequent requests to the same pool using a cookie.
	// "IPCookie" — like Cookie but based on client IP address.
	// "Header" — route based on a request header value.
	// +optional
	// +kubebuilder:validation:Enum=None;Cookie;IPCookie;Header
	SessionAffinity string `json:"sessionAffinity,omitempty"`

	// Monitor configures health checking for tunnel origins.
	// +optional
	Monitor *LoadBalancerMonitorConfig `json:"monitor,omitempty"`
}

// LoadBalancerMonitorConfig configures health checking for tunnel origins.
type LoadBalancerMonitorConfig struct {
	// Type is the health check protocol: HTTP, HTTPS, or TCP.
	// Default: HTTPS.
	// +optional
	// +kubebuilder:validation:Enum=HTTP;HTTPS;TCP
	Type string `json:"type,omitempty"`

	// Path is the health check endpoint path.
	// Default: /ready.
	// +optional
	Path string `json:"path,omitempty"`

	// Interval is the number of seconds between health checks.
	// Default: 60.
	// +optional
	Interval int64 `json:"interval,omitempty"`

	// Timeout is the number of seconds before marking a health check as failed.
	// Default: 5.
	// +optional
	Timeout int64 `json:"timeout,omitempty"`
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
