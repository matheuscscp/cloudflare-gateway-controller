// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// reconcileAllTunnelIngress builds and applies ingress rules for all tunnel
// entries. Returns the denied refs map, change messages, and any error.
func (r *GatewayReconciler) reconcileAllTunnelIngress(ctx context.Context, tc cloudflare.Client, entries []tunnelEntry, routes []*gatewayv1.HTTPRoute) (map[types.NamespacedName][]string, []string, error) {
	l := log.FromContext(ctx)

	ingress, routesWithDeniedRefs, err := buildIngressRules(ctx, r.Client, routes)
	if err != nil {
		return nil, nil, err
	}
	if len(routesWithDeniedRefs) > 0 {
		l.V(1).Info("BackendRefs denied due to missing or failed ReferenceGrant checks", "routes", len(routesWithDeniedRefs))
	}

	var changes []string
	for i := range entries {
		e := &entries[i]

		currentIngress, err := tc.GetTunnelConfiguration(ctx, e.tunnelID)
		if err != nil {
			return nil, nil, fmt.Errorf("getting tunnel %q configuration: %w", e.tunnelName, err)
		}
		if !ingressRulesEqual(currentIngress, ingress) {
			if err := tc.UpdateTunnelConfiguration(ctx, e.tunnelID, ingress); err != nil {
				return nil, nil, fmt.Errorf("updating tunnel %q configuration: %w", e.tunnelName, err)
			}
			changes = append(changes, fmt.Sprintf("updated tunnel %s configuration", e.tunnelName))
			l.V(1).Info("Updated tunnel configuration", "tunnelName", e.tunnelName)
		}
	}
	return routesWithDeniedRefs, changes, nil
}

// buildIngressRules converts a list of HTTPRoutes into Cloudflare tunnel ingress rules.
// For each rule in each route it takes the first backendRef and maps every hostname to
// http://<service>.<namespace>.svc.cluster.local:<port>, optionally with a path prefix.
// Only the first backendRef per rule is used because additional backendRefs represent
// traffic splitting (weighted load balancing), which Cloudflare tunnel ingress does not
// support — use a Kubernetes Service for that instead.
// Cross-namespace backendRefs are validated against ReferenceGrants. Routes with denied
// refs are returned in a map (route name -> list of denied ref names) so the caller can
// set ResolvedRefs=False on those routes with a specific message.
// A catch-all 404 rule is appended.
func buildIngressRules(ctx context.Context, r client.Reader, routes []*gatewayv1.HTTPRoute) ([]cloudflare.IngressRule, map[types.NamespacedName][]string, error) {
	var rules []cloudflare.IngressRule
	routesWithDeniedRefs := make(map[types.NamespacedName][]string)
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			if len(rule.BackendRefs) == 0 {
				continue
			}
			ref := rule.BackendRefs[0]
			ns := route.Namespace
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}
			granted, err := backendReferenceGranted(ctx, r, route.Namespace, ns, string(ref.Name))
			if err != nil {
				return nil, nil, fmt.Errorf("checking ReferenceGrant for backendRef %s/%s in HTTPRoute %s/%s: %w", ns, ref.Name, route.Namespace, route.Name, err)
			}
			if !granted {
				key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
				routesWithDeniedRefs[key] = append(routesWithDeniedRefs[key], ns+"/"+string(ref.Name))
				continue
			}
			port := int32(80)
			if ref.Port != nil {
				port = *ref.Port
			}
			service := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", string(ref.Name), ns, port)
			path := pathFromMatches(rule.Matches)
			for _, hostname := range route.Spec.Hostnames {
				rules = append(rules, cloudflare.IngressRule{
					Hostname: string(hostname),
					Service:  service,
					Path:     path,
				})
			}
		}
	}
	// Append catch-all rule.
	rules = append(rules, cloudflare.IngressRule{
		Service: "http_status:404",
	})
	return rules, routesWithDeniedRefs, nil
}

// pathFromMatches extracts a path prefix from the first HTTPRouteMatch that has
// a PathPrefix match. Returns empty string if no path match is specified.
func pathFromMatches(matches []gatewayv1.HTTPRouteMatch) string {
	for _, m := range matches {
		if m.Path == nil {
			continue
		}
		if m.Path.Type == nil || *m.Path.Type == gatewayv1.PathMatchPathPrefix {
			if m.Path.Value != nil {
				return *m.Path.Value
			}
		}
	}
	return ""
}

// ingressRulesEqual reports whether two slices of ingress rules contain the
// same rules regardless of order. It sorts copies of both slices before comparing.
func ingressRulesEqual(a, b []cloudflare.IngressRule) bool {
	cmp := func(x, y cloudflare.IngressRule) int {
		if c := strings.Compare(x.Hostname, y.Hostname); c != 0 {
			return c
		}
		if c := strings.Compare(x.Service, y.Service); c != 0 {
			return c
		}
		return strings.Compare(x.Path, y.Path)
	}
	sortedA := slices.Clone(a)
	sortedB := slices.Clone(b)
	slices.SortFunc(sortedA, cmp)
	slices.SortFunc(sortedB, cmp)
	return slices.Equal(sortedA, sortedB)
}
