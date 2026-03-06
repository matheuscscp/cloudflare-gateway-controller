// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/proxy"
)

// routeObject abstracts HTTPRoute and GRPCRoute for shared reconciliation logic.
// The obj() method returns the underlying scheme-registered object for API calls.
type routeObject interface {
	// obj returns the underlying client.Object (HTTPRoute or GRPCRoute) for API calls.
	obj() client.Object
	// routeKind returns the kind string (e.g. "HTTPRoute", "GRPCRoute").
	routeKind() string
	// key returns the namespaced name of the route.
	key() types.NamespacedName
	// generation returns the route's metadata generation.
	generation() int64
	// parentRefs returns spec.parentRefs.
	parentRefs() []gatewayv1.ParentReference
	// hostnames returns spec.hostnames.
	hostnames() []gatewayv1.Hostname
	// routeParents returns a pointer to the route's status.parents slice
	// for reading and in-place mutation.
	routeParents() *[]gatewayv1.RouteParentStatus
	// deepCopy returns a deep copy of the underlying object for merge-patch.
	deepCopy() client.Object
	// validate checks for unsupported features, returning issue descriptions.
	validate() []string
	// hostnamePathKeys returns (hostname, pathPrefix) keys for conflict detection.
	hostnamePathKeys() []hostnamePathKey
	// buildProxyRoutes converts the route's rules into proxy.Route entries,
	// checking ReferenceGrants for cross-namespace backends.
	buildProxyRoutes(ctx context.Context, r client.Reader) (routes []proxy.Route, deniedRefs []string, err error)
}

// httpRouteObject wraps an HTTPRoute to implement routeObject.
type httpRouteObject struct {
	route *gatewayv1.HTTPRoute
}

func (h *httpRouteObject) obj() client.Object                      { return h.route }
func (h *httpRouteObject) routeKind() string                       { return apiv1.KindHTTPRoute }
func (h *httpRouteObject) key() types.NamespacedName               { return client.ObjectKeyFromObject(h.route) }
func (h *httpRouteObject) generation() int64                       { return h.route.Generation }
func (h *httpRouteObject) parentRefs() []gatewayv1.ParentReference { return h.route.Spec.ParentRefs }
func (h *httpRouteObject) hostnames() []gatewayv1.Hostname         { return h.route.Spec.Hostnames }
func (h *httpRouteObject) routeParents() *[]gatewayv1.RouteParentStatus {
	return &h.route.Status.Parents
}
func (h *httpRouteObject) deepCopy() client.Object { return h.route.DeepCopy() }
func (h *httpRouteObject) validate() []string      { return validateHTTPRoute(h.route) }

func (h *httpRouteObject) hostnamePathKeys() []hostnamePathKey {
	keys := make([]hostnamePathKey, 0, len(h.route.Spec.Rules)*len(h.route.Spec.Hostnames))
	for _, rule := range h.route.Spec.Rules {
		path := pathFromMatches(rule.Matches)
		for _, hostname := range h.route.Spec.Hostnames {
			keys = append(keys, hostnamePathKey{hostname: string(hostname), path: path})
		}
	}
	return keys
}

func (h *httpRouteObject) buildProxyRoutes(ctx context.Context, r client.Reader) ([]proxy.Route, []string, error) {
	owner := h.route.Namespace + "/" + h.route.Name
	var routes []proxy.Route
	var allDeniedRefs []string
	for _, rule := range h.route.Spec.Rules {
		if len(rule.BackendRefs) == 0 {
			continue
		}
		var backends []proxy.Backend
		denied := false
		for _, ref := range rule.BackendRefs {
			backend, deniedRef, err := resolveBackend(ctx, r, h.routeKind(), h.route.Namespace, h.route.Name, ref.BackendRef)
			if err != nil {
				return nil, nil, err
			}
			if deniedRef != "" {
				allDeniedRefs = append(allDeniedRefs, deniedRef)
				denied = true
				continue
			}
			backends = append(backends, *backend)
		}
		if denied || len(backends) == 0 {
			continue
		}
		sp := buildSessionPersistence(rule.SessionPersistence)
		pathPrefix := pathFromMatches(rule.Matches)
		for _, hostname := range h.route.Spec.Hostnames {
			routes = append(routes, proxy.Route{
				Hostname:           string(hostname),
				PathPrefix:         pathPrefix,
				Owner:              owner,
				Backends:           backends,
				SessionPersistence: sp,
			})
		}
	}
	return routes, allDeniedRefs, nil
}

// grpcRouteObject wraps a GRPCRoute to implement routeObject.
type grpcRouteObject struct {
	route *gatewayv1.GRPCRoute
}

func (g *grpcRouteObject) obj() client.Object                      { return g.route }
func (g *grpcRouteObject) routeKind() string                       { return apiv1.KindGRPCRoute }
func (g *grpcRouteObject) key() types.NamespacedName               { return client.ObjectKeyFromObject(g.route) }
func (g *grpcRouteObject) generation() int64                       { return g.route.Generation }
func (g *grpcRouteObject) parentRefs() []gatewayv1.ParentReference { return g.route.Spec.ParentRefs }
func (g *grpcRouteObject) hostnames() []gatewayv1.Hostname         { return g.route.Spec.Hostnames }
func (g *grpcRouteObject) routeParents() *[]gatewayv1.RouteParentStatus {
	return &g.route.Status.Parents
}
func (g *grpcRouteObject) deepCopy() client.Object { return g.route.DeepCopy() }
func (g *grpcRouteObject) validate() []string      { return validateGRPCRoute(g.route) }

func (g *grpcRouteObject) hostnamePathKeys() []hostnamePathKey {
	keys := make([]hostnamePathKey, 0, len(g.route.Spec.Rules)*len(g.route.Spec.Hostnames))
	for range g.route.Spec.Rules {
		for _, hostname := range g.route.Spec.Hostnames {
			keys = append(keys, hostnamePathKey{hostname: string(hostname), path: "", protocol: proxy.ProtocolGRPC})
		}
	}
	return keys
}

func (g *grpcRouteObject) buildProxyRoutes(ctx context.Context, r client.Reader) ([]proxy.Route, []string, error) {
	owner := g.route.Namespace + "/" + g.route.Name
	var routes []proxy.Route
	var allDeniedRefs []string
	for _, rule := range g.route.Spec.Rules {
		if len(rule.BackendRefs) == 0 {
			continue
		}
		var backends []proxy.Backend
		denied := false
		for _, ref := range rule.BackendRefs {
			backend, deniedRef, err := resolveBackend(ctx, r, g.routeKind(), g.route.Namespace, g.route.Name, ref.BackendRef)
			if err != nil {
				return nil, nil, err
			}
			if deniedRef != "" {
				allDeniedRefs = append(allDeniedRefs, deniedRef)
				denied = true
				continue
			}
			backends = append(backends, *backend)
		}
		if denied || len(backends) == 0 {
			continue
		}
		sp := buildSessionPersistence(rule.SessionPersistence)
		for _, hostname := range g.route.Spec.Hostnames {
			routes = append(routes, proxy.Route{
				Hostname:           string(hostname),
				Protocol:           proxy.ProtocolGRPC,
				Owner:              owner,
				Backends:           backends,
				SessionPersistence: sp,
			})
		}
	}
	return routes, allDeniedRefs, nil
}

// validateGRPCRoute checks that the GRPCRoute only uses features supported by
// Cloudflare tunnels. Returns a list of unsupported feature descriptions,
// or nil if valid.
func validateGRPCRoute(route *gatewayv1.GRPCRoute) []string {
	var issues []string
	for i, ref := range route.Spec.ParentRefs {
		if ref.Port != nil {
			issues = append(issues, fmt.Sprintf("spec.parentRefs[%d].port is not supported", i))
		}
	}
	for i, rule := range route.Spec.Rules {
		if len(rule.Filters) > 0 {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].filters is not supported", i))
		}
		if sp := rule.SessionPersistence; sp != nil {
			if sp.IdleTimeout != nil && sp.Type != nil && *sp.Type == gatewayv1.HeaderBasedSessionPersistence {
				issues = append(issues, fmt.Sprintf(
					"spec.rules[%d].sessionPersistence.idleTimeout is not supported for header-based sessions", i))
			}
		}
		if len(rule.BackendRefs) == 0 {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs: at least one backend is required", i))
		}
		for j, ref := range rule.BackendRefs {
			if len(ref.Filters) > 0 {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs[%d].filters is not supported", i, j))
			}
			group := ""
			if ref.Group != nil {
				group = string(*ref.Group)
			}
			kind := apiv1.KindService
			if ref.Kind != nil {
				kind = string(*ref.Kind)
			}
			if group != "" || kind != apiv1.KindService {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs[%d]: only core Service backends are supported, got group=%q kind=%q", i, j, group, kind))
			}
		}
		for j, m := range rule.Matches {
			if m.Method != nil {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].matches[%d].method is not supported", i, j))
			}
			if len(m.Headers) > 0 {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].matches[%d].headers is not supported", i, j))
			}
		}
	}
	return issues
}

// resolveBackend resolves a single BackendRef into a proxy.Backend, checking
// ReferenceGrants for cross-namespace references. Returns the backend (nil if
// denied), the denied ref description (empty if granted), and any error.
func resolveBackend(ctx context.Context, r client.Reader, routeKind, routeNamespace, routeName string, ref gatewayv1.BackendRef) (*proxy.Backend, string, error) {
	ns := routeNamespace
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}
	granted, err := backendReferenceGranted(ctx, r, routeKind, routeNamespace, ns, string(ref.Name))
	if err != nil {
		return nil, "", fmt.Errorf("checking ReferenceGrant for backendRef %s/%s in %s %s/%s: %w", ns, ref.Name, routeKind, routeNamespace, routeName, err)
	}
	if !granted {
		return nil, ns + "/" + string(ref.Name), nil
	}
	port := int32(80)
	if ref.Port != nil {
		port = *ref.Port
	}
	weight := int32(1)
	if ref.Weight != nil {
		weight = *ref.Weight
	}
	service := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", string(ref.Name), ns, port)
	return &proxy.Backend{Service: service, Weight: weight}, "", nil
}

// buildSessionPersistence converts a Gateway API SessionPersistence spec into
// the proxy config equivalent.
func buildSessionPersistence(sp *gatewayv1.SessionPersistence) *proxy.SessionPersistence {
	if sp == nil {
		return nil
	}
	spType := "Cookie"
	if sp.Type != nil {
		spType = string(*sp.Type)
	}
	sessionName := "cgw-session"
	if spType == "Header" {
		sessionName = "X-Cgw-Session"
	}
	if sp.SessionName != nil {
		sessionName = *sp.SessionName
	}
	result := &proxy.SessionPersistence{
		Type:        spType,
		SessionName: sessionName,
	}
	if sp.AbsoluteTimeout != nil {
		result.AbsoluteTimeout = string(*sp.AbsoluteTimeout)
	}
	if sp.IdleTimeout != nil {
		result.IdleTimeout = string(*sp.IdleTimeout)
	}
	if sp.CookieConfig != nil && sp.CookieConfig.LifetimeType != nil {
		result.CookieLifetimeType = string(*sp.CookieConfig.LifetimeType)
	} else {
		result.CookieLifetimeType = "Session"
	}
	return result
}

// listenerAllowsKind reports whether the given listener allows routes of
// the specified kind. When allowedRoutes.kinds is empty, both HTTPRoute
// and GRPCRoute are allowed (matching the controller's supported kinds).
func listenerAllowsKind(l gatewayv1.Listener, kind string) bool {
	if l.AllowedRoutes == nil || len(l.AllowedRoutes.Kinds) == 0 {
		return kind == apiv1.KindHTTPRoute || kind == apiv1.KindGRPCRoute
	}
	for _, k := range l.AllowedRoutes.Kinds {
		group := gatewayv1.Group(gatewayv1.GroupName)
		if k.Group != nil {
			group = *k.Group
		}
		if group == gatewayv1.Group(gatewayv1.GroupName) && string(k.Kind) == kind {
			return true
		}
	}
	return false
}
