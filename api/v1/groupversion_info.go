// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// +groupName=cloudflare-gateway-controller.io
// +kubebuilder:object:generate=true
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Controller names.
const (
	// ControllerName is the identifier used in GatewayClass.spec.controllerName.
	ControllerName = gatewayv1.GatewayController(ShortControllerName + ".io/controller")

	// ShortControllerName is a shorter identifier used in events and conditions.
	ShortControllerName = "cloudflare-gateway-controller"
)

// Group is the API group for this project.
const Group = ShortControllerName + ".io"

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionResource scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: SchemeGroupVersion}

	// Install adds the types in this group-version to the given scheme.
	Install = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&CloudflareGatewayParameters{},
		&CloudflareGatewayParametersList{})
}
