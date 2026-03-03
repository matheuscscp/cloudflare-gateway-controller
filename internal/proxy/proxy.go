// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

// HeaderConfigNotLoaded is set on responses when the proxy has no configuration
// loaded yet. The health check uses this to distinguish an internal 503 from a
// proxied one.
const HeaderConfigNotLoaded = "X-Proxy-Config-Not-Loaded"

// noKeepAliveTransport is a shared http.Transport with keep-alives disabled.
// Each request opens a fresh TCP connection through kube-proxy.
var noKeepAliveTransport = &http.Transport{DisableKeepAlives: true}

// h2cTransport is a shared HTTP/2 cleartext transport for gRPC backends.
var h2cTransport http.RoundTripper = &http2.Transport{
	AllowHTTP: true,
	DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	},
}

// Proxy is a reverse proxy that routes requests based on hostname and path
// prefix matching. The routing table is swapped atomically when the ConfigMap
// watcher detects a change.
type Proxy struct {
	config atomic.Pointer[Config]
}

// SetConfig atomically replaces the routing table.
func (p *Proxy) SetConfig(cfg *Config) {
	p.config.Store(cfg)
}

// resolveBackend matches a request to a route and picks a backend.
// Returns the route, backend, whether a session was hit, and the
// session createdAt timestamp (for cookie re-issue).
func (p *Proxy) resolveBackend(r *http.Request) (*Route, *Backend, bool, int64, error) {
	cfg := p.config.Load()
	if cfg == nil {
		return nil, nil, false, 0, errConfigNotLoaded
	}

	route := matchRoute(cfg, r.Host, r.URL.Path)
	if route == nil {
		return nil, nil, false, 0, errNoRoute
	}

	// Session persistence: try to resolve an existing session.
	var backend *Backend
	var sessionHit bool
	var sessionCreatedAt int64
	if route.SessionPersistence != nil {
		backend, sessionCreatedAt, sessionHit = resolveSession(route, r)
	}

	// Fall back to weighted random selection.
	if backend == nil {
		backend = pickBackend(route)
	}
	if backend == nil {
		return nil, nil, false, 0, errNoBackend
	}

	return route, backend, sessionHit, sessionCreatedAt, nil
}

var (
	errConfigNotLoaded = fmt.Errorf("no configuration loaded")
	errNoRoute         = fmt.Errorf("no matching route")
	errNoBackend       = fmt.Errorf("no available backend")
)

// ServeHTTP implements http.Handler. It matches the request by hostname (exact)
// then longest PathPrefix, picks a weighted backend, and forwards the request
// with DisableKeepAlives so each request opens a fresh TCP connection through
// kube-proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, backend, sessionHit, sessionCreatedAt, err := p.resolveBackend(r)
	if err != nil {
		switch err {
		case errConfigNotLoaded:
			w.Header().Set(HeaderConfigNotLoaded, "true")
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		case errNoRoute:
			http.NotFound(w, r)
		case errNoBackend:
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.serviceURL.Scheme
			req.URL.Host = backend.serviceURL.Host
		},
		Transport: transportForRoute(route),
	}

	// Set or re-issue cookie (cookie-based persistence only).
	// Re-issue on every response when idleTimeout is configured to refresh lastActivity.
	if sp := route.SessionPersistence; sp != nil && sp.Type == "Cookie" && (!sessionHit || sp.idleTimeout > 0) {
		backendID := backend.id
		createdAt := sessionCreatedAt
		if !sessionHit {
			createdAt = time.Now().Unix()
		}
		cookiePath := route.PathPrefix
		if cookiePath == "" {
			cookiePath = "/"
		}
		proxy.ModifyResponse = func(resp *http.Response) error {
			cookie := &http.Cookie{
				Name:     sp.SessionName,
				Value:    sessionCookieValue(backendID, createdAt, sp.idleTimeout > 0),
				Path:     cookiePath,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			}
			if sp.CookieLifetimeType == "Permanent" {
				cookie.MaxAge = cookieMaxAge(sp, createdAt)
			}
			if v := cookie.String(); v != "" {
				resp.Header.Add("Set-Cookie", v)
			}
			return nil
		}
	}

	proxy.ServeHTTP(w, r)
}

// RoundTrip implements http.RoundTripper. It resolves the backend using the
// same routing logic as ServeHTTP, rewrites the request URL, and performs a
// direct HTTP round-trip to the backend. This is used by the tunnel's origin
// proxy for WebSocket upgrades, where httputil.ReverseProxy's Hijack-based
// handling doesn't work with QUIC streams.
func (p *Proxy) RoundTrip(r *http.Request) (*http.Response, error) {
	route, backend, _, _, err := p.resolveBackend(r)
	if err != nil {
		return nil, err
	}

	r.URL.Scheme = backend.serviceURL.Scheme
	r.URL.Host = backend.serviceURL.Host
	return transportForRoute(route).RoundTrip(r)
}

// transportForRoute returns the appropriate HTTP transport for the route's protocol.
func transportForRoute(route *Route) http.RoundTripper {
	if route.Protocol == "h2c" {
		return h2cTransport
	}
	return noKeepAliveTransport
}

// resolveSession attempts to find the backend for an existing session.
// Returns the backend, the original createdAt timestamp (for cookie re-issue),
// and true if the session is valid, or nil/0/false otherwise.
func resolveSession(route *Route, r *http.Request) (*Backend, int64, bool) {
	sp := route.SessionPersistence

	switch sp.Type {
	case "Cookie":
		cookie, err := r.Cookie(sp.SessionName)
		if err != nil {
			return nil, 0, false
		}
		backendID, createdAt, lastActivity, ok := parseSessionCookie(cookie.Value)
		if !ok {
			return nil, 0, false
		}
		now := time.Now().Unix()
		// Check absolute timeout.
		if sp.absoluteTimeout > 0 {
			elapsed := time.Duration(now-createdAt) * time.Second
			if elapsed > sp.absoluteTimeout {
				return nil, 0, false
			}
		}
		// Check idle timeout.
		if sp.idleTimeout > 0 {
			elapsed := time.Duration(now-lastActivity) * time.Second
			if elapsed > sp.idleTimeout {
				return nil, 0, false
			}
		}
		backend := findBackendByID(route, backendID)
		if backend == nil {
			return nil, 0, false
		}
		return backend, createdAt, true

	case "Header":
		backendID := r.Header.Get(sp.SessionName)
		if backendID == "" {
			return nil, 0, false
		}
		backend := findBackendByID(route, backendID)
		if backend == nil {
			return nil, 0, false
		}
		return backend, 0, true
	}

	return nil, 0, false
}

// findBackendByID finds a backend with the given ID and weight > 0.
func findBackendByID(route *Route, id string) *Backend {
	for i := range route.Backends {
		if route.Backends[i].id == id && route.Backends[i].Weight > 0 {
			return &route.Backends[i]
		}
	}
	return nil
}

// sessionCookieValue creates a cookie value. When withLastActivity is false the
// format is "backendID.createdAt"; when true it is "backendID.createdAt.now"
// where now is the current Unix timestamp used as the last-activity marker.
func sessionCookieValue(backendID string, createdAt int64, withLastActivity bool) string {
	if withLastActivity {
		return backendID + "." + strconv.FormatInt(createdAt, 10) + "." + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return backendID + "." + strconv.FormatInt(createdAt, 10)
}

// parseSessionCookie parses a cookie value in the format "backendID.createdAt"
// or "backendID.createdAt.lastActivity". When the lastActivity segment is
// absent, lastActivity is set equal to createdAt.
func parseSessionCookie(value string) (backendID string, createdAt, lastActivity int64, ok bool) {
	// First cut: backendID . rest
	id, rest, found := strings.Cut(value, ".")
	if !found {
		return "", 0, 0, false
	}

	// Second cut: createdAt [ . lastActivity ]
	createdStr, lastActStr, hasLastAct := strings.Cut(rest, ".")

	created, err := strconv.ParseInt(createdStr, 10, 64)
	if err != nil {
		return "", 0, 0, false
	}

	if !hasLastAct {
		return id, created, created, true
	}

	// Reject extra dots (e.g. "a.1.2.3").
	if strings.ContainsRune(lastActStr, '.') {
		return "", 0, 0, false
	}

	lastAct, err := strconv.ParseInt(lastActStr, 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return id, created, lastAct, true
}

// cookieMaxAge computes the Max-Age for a Permanent cookie. When only
// absoluteTimeout is set, Max-Age equals absoluteTimeout. When only idleTimeout
// is set, Max-Age equals idleTimeout. When both are set, Max-Age is the minimum
// of the remaining absolute timeout and idleTimeout.
func cookieMaxAge(sp *SessionPersistence, createdAt int64) int {
	abs := sp.absoluteTimeout > 0
	idle := sp.idleTimeout > 0
	switch {
	case abs && idle:
		remaining := int(sp.absoluteTimeout.Seconds()) - int(time.Now().Unix()-createdAt)
		idleSec := int(sp.idleTimeout.Seconds())
		if remaining < idleSec {
			return remaining
		}
		return idleSec
	case abs:
		return int(sp.absoluteTimeout.Seconds())
	case idle:
		return int(sp.idleTimeout.Seconds())
	default:
		return 0
	}
}

// pickBackend selects a backend from the route using weighted random selection.
// Backends with weight 0 are never selected. Returns nil if no backend is
// available (empty slice or all weights are 0).
func pickBackend(route *Route) *Backend {
	if route.totalWeight <= 0 {
		return nil
	}

	r := rand.Int32N(route.totalWeight)
	var cumulative int32
	for i := range route.Backends {
		cumulative += route.Backends[i].Weight
		if r < cumulative {
			return &route.Backends[i]
		}
	}
	return nil // unreachable
}

// matchRoute finds the best matching route: exact hostname match via the
// precomputed index, then longest PathPrefix. Returns nil if no route matches.
func matchRoute(cfg *Config, host, path string) *Route {
	// Strip port from host if present.
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}

	routes := cfg.routesByHost[host]
	var best *Route
	bestLen := -1
	for _, r := range routes {
		prefix := r.PathPrefix
		if prefix == "" {
			prefix = "/"
		}
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		if len(prefix) > bestLen {
			best = r
			bestLen = len(prefix)
		}
	}
	return best
}
