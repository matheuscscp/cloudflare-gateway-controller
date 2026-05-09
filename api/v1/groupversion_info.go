// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// +groupName=cloudflare-gateway-controller.io
// +kubebuilder:object:generate=true
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Controller names.
const (
	// ControllerName is the identifier used in GatewayClass.spec.controllerName.
	ControllerName = gatewayv1.GatewayController(Group + "/controller")

	// ShortControllerName is a shorter identifier used in events and conditions.
	ShortControllerName = "cloudflare-gateway-controller"
)

// Groups.
const (
	// Group is the API group for this project.
	Group = ShortControllerName + ".io"

	// GroupGateways is the API group of the project scoped to Gateways.
	GroupGateways = "gateways." + Group

	// GroupCore is the Kubernetes core API group name used in Gateway API
	// references (e.g. parametersRef.group).
	GroupCore = "core"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionResource scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// Install adds the types in this group-version to the given scheme.
	Install = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&CloudflareGatewayParameters{},
		&CloudflareGatewayParametersList{},
		&CloudflareGatewayStatus{},
		&CloudflareGatewayStatusList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
