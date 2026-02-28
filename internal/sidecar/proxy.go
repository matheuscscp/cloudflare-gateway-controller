// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar

import (
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
// then longest PathPrefix, and forwards the request to the matched backend with
// DisableKeepAlives so each request opens a fresh TCP connection through kube-proxy.
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

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = route.serviceURL.Scheme
			req.URL.Host = route.serviceURL.Host
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	proxy.ServeHTTP(w, r)
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
