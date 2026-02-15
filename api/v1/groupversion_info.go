// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// +groupName=cloudflare-gateway-controller.matheuscscp.github.com
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ControllerName is the identifier used in GatewayClass.spec.controllerName.
const ControllerName = "github.com/matheuscscp/cloudflare-gateway-controller"

// Group is the API group for this version.
const Group = "cloudflare-gateway-controller.matheuscscp.github.com"

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1"}
)
