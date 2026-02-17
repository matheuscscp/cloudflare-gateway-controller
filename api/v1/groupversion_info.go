// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// +groupName=cloudflare-gateway-controller.matheuscscp.github.com
package v1

import (
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Controller names.
const (
	// ControllerName is the identifier used in GatewayClass.spec.controllerName.
	ControllerName = gatewayv1.GatewayController("github.com/matheuscscp/" + ShortControllerName)

	// ShortControllerName is a shorter identifier used in events and conditions.
	ShortControllerName = "cloudflare-gateway-controller"
)

// Group is the API group for this project.
const Group = ShortControllerName + ".matheuscscp.github.com"

// Group prefixes.
const (
	prefix        = Group + "/"
	prefixGateway = "gateway." + prefix
)
