// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

// secretReferenceGranted checks whether a Gateway in gatewayNamespace is allowed
// to reference a Secret in secretNamespace. Returns true if both namespaces are the
// same or if a ReferenceGrant in the Secret's namespace permits the cross-namespace
// reference.
func secretReferenceGranted(ctx context.Context, r client.Reader, gatewayNamespace, secretNamespace, secretName string) (bool, error) {
	if gatewayNamespace == secretNamespace {
		return true, nil
	}

	var grants gatewayv1beta1.ReferenceGrantList
	if err := r.List(ctx, &grants, client.InNamespace(secretNamespace)); err != nil {
		return false, fmt.Errorf("listing ReferenceGrants in namespace %s: %w", secretNamespace, err)
	}

	for i := range grants.Items {
		grant := &grants.Items[i]
		fromMatch := false
		for _, from := range grant.Spec.From {
			if from.Group == gatewayv1beta1.Group(gatewayv1.GroupName) &&
				from.Kind == gatewayv1beta1.Kind(apiv1.KindGateway) &&
				string(from.Namespace) == gatewayNamespace {
				fromMatch = true
				break
			}
		}
		if !fromMatch {
			continue
		}
		for _, to := range grant.Spec.To {
			if to.Group == gatewayv1beta1.Group("") && to.Kind == gatewayv1beta1.Kind(apiv1.KindSecret) {
				if to.Name == nil || string(*to.Name) == secretName {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// backendReferenceGranted checks whether an HTTPRoute in routeNamespace is allowed
// to reference a Service in serviceNamespace. Returns true if both namespaces are
// the same or if a ReferenceGrant in the Service's namespace permits the
// cross-namespace reference.
func backendReferenceGranted(ctx context.Context, r client.Reader, routeNamespace, serviceNamespace, serviceName string) (bool, error) {
	if routeNamespace == serviceNamespace {
		return true, nil
	}

	var grants gatewayv1beta1.ReferenceGrantList
	if err := r.List(ctx, &grants, client.InNamespace(serviceNamespace)); err != nil {
		return false, fmt.Errorf("listing ReferenceGrants in namespace %s: %w", serviceNamespace, err)
	}

	for i := range grants.Items {
		grant := &grants.Items[i]
		fromMatch := false
		for _, from := range grant.Spec.From {
			if from.Group == gatewayv1beta1.Group(gatewayv1.GroupName) &&
				from.Kind == gatewayv1beta1.Kind(apiv1.KindHTTPRoute) &&
				string(from.Namespace) == routeNamespace {
				fromMatch = true
				break
			}
		}
		if !fromMatch {
			continue
		}
		for _, to := range grant.Spec.To {
			if to.Group == gatewayv1beta1.Group("") && to.Kind == gatewayv1beta1.Kind(apiv1.KindService) {
				if to.Name == nil || string(*to.Name) == serviceName {
					return true, nil
				}
			}
		}
	}

	return false, nil
}
