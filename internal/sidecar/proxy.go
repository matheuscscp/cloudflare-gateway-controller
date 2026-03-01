// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HeaderConfigNotLoaded is set on responses when the proxy has no configuration
// loaded yet. The health check uses this to distinguish an internal 503 from a
// proxied one.
const HeaderConfigNotLoaded = "X-Sidecar-Config-Not-Loaded"

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

// ServeHTTP implements http.Handler. It matches the request by hostname (exact)
// then longest PathPrefix, picks a weighted backend, and forwards the request
// with DisableKeepAlives so each request opens a fresh TCP connection through
// kube-proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := p.config.Load()
	if cfg == nil {
		w.Header().Set(HeaderConfigNotLoaded, "true")
		http.Error(w, "no configuration loaded", http.StatusServiceUnavailable)
		return
	}

	route := matchRoute(cfg.Routes, r.Host, r.URL.Path)
	if route == nil {
		http.NotFound(w, r)
		return
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
		http.Error(w, "no available backend", http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.serviceURL.Scheme
			req.URL.Host = backend.serviceURL.Host
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
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
		return fmt.Sprintf("%s.%d.%d", backendID, createdAt, time.Now().Unix())
	}
	return fmt.Sprintf("%s.%d", backendID, createdAt)
}

// parseSessionCookie parses a cookie value in the format "backendID.createdAt"
// or "backendID.createdAt.lastActivity". When the lastActivity segment is
// absent, lastActivity is set equal to createdAt.
func parseSessionCookie(value string) (backendID string, createdAt, lastActivity int64, ok bool) {
	parts := strings.Split(value, ".")
	switch len(parts) {
	case 2:
		ts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return "", 0, 0, false
		}
		return parts[0], ts, ts, true
	case 3:
		created, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return "", 0, 0, false
		}
		lastAct, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return "", 0, 0, false
		}
		return parts[0], created, lastAct, true
	default:
		return "", 0, 0, false
	}
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
	var totalWeight int32
	for i := range route.Backends {
		totalWeight += route.Backends[i].Weight
	}
	if totalWeight <= 0 {
		return nil
	}

	r := rand.Int32N(totalWeight)
	var cumulative int32
	for i := range route.Backends {
		cumulative += route.Backends[i].Weight
		if r < cumulative {
			return &route.Backends[i]
		}
	}
	return nil // unreachable
}

// matchRoute finds the best matching route: exact hostname match, then
// longest PathPrefix. Returns nil if no route matches.
func matchRoute(routes []Route, host, path string) *Route {
	// Strip port from host if present.
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}

	var best *Route
	bestLen := -1
	for i := range routes {
		r := &routes[i]
		if r.Hostname != host {
			continue
		}
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
