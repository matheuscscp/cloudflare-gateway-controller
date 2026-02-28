// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar

import (
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync/atomic"
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

	backend := pickBackend(route)
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
	proxy.ServeHTTP(w, r)
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
