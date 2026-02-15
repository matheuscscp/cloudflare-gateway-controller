// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// +groupName=cloudflare-gateway-controller.matheuscscp.github.com
package v1

// ControllerName is the identifier used in GatewayClass.spec.controllerName.
const ControllerName = "github.com/matheuscscp/cloudflare-gateway-controller"

// Group is the API group for this project.
const Group = "cloudflare-gateway-controller.matheuscscp.github.com"

// Group prefixes.
const (
	prefix        = Group + "/"
	prefixGateway = "gateway." + prefix
)
