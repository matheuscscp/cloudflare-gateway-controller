// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"golang.org/x/net/http2"
)

const (
	// HeaderConfigNotLoaded is set on responses when the proxy has no configuration
	// loaded yet. The health check uses this to distinguish an internal 503 from a
	// proxied one.
	HeaderConfigNotLoaded = "X-Proxy-Config-Not-Loaded"
)

const (
	// FlagNamespace is the tunnel command flag for the namespace.
	FlagNamespace = "namespace"

	// FlagConfigMapName is the tunnel command flag for the ConfigMap name.
	FlagConfigMapName = "configmap-name"
)

const (
	// HealthPath is the HTTP path used by all probes (startup, liveness,
	// readiness). The tunnel serves a single handler at this path.
	HealthPath = "/healthz"
)

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

	// errorLog is used by httputil.ReverseProxy for proxy error messages.
	// When nil, the default log package logger is used.
	errorLog *log.Logger
}

// NewProxy creates a Proxy with the given logger.
func NewProxy(zlog *zerolog.Logger) *Proxy {
	return &Proxy{
		errorLog: log.New(zlog, "", 0),
	}
}

// StartHealthServer starts the health and Prometheus metrics server on addr.
// The cloudflaredHealth handler is called after the proxy health check passes
// to check cloudflared's own connection state. The server is shut down when
// ctx is cancelled.
func (p *Proxy) StartHealthServer(ctx context.Context, addr string, cloudflaredHealth http.Handler) {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if p.config.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		cloudflaredHealth.ServeHTTP(w, r)
	})
	mux.Handle("/metrics", promhttp.InstrumentMetricHandler(
		prometheus.DefaultRegisterer, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
			EnableOpenMetrics: true,
			ProcessStartTime:  time.Now(),
		}),
	))
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			p.errorLog.Fatalf("health server error: %v", err)
		}
	}()
}

// SetConfig atomically replaces the routing table.
func (p *Proxy) SetConfig(cfg *Config) {
	p.config.Store(cfg)
}

// isGRPC reports whether the request is a gRPC request by checking if the
// Content-Type header starts with "application/grpc".
func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// resolveBackend matches a request to a route and picks a backend.
// Returns the route, backend, whether a session was hit, and the
// session createdAt timestamp (for cookie re-issue).
func (p *Proxy) resolveBackend(r *http.Request) (*Route, *Backend, bool, int64, error) {
	cfg := p.config.Load()
	if cfg == nil {
		return nil, nil, false, 0, errConfigNotLoaded
	}

	route := matchRoute(cfg, r.Host, r.URL.Path, isGRPC(r))
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

// ServeHTTP implements http.Handler. It is called by cloudflared's origin proxy
// for regular HTTP requests (not WebSocket or gRPC). It matches the request by
// hostname (exact) then longest PathPrefix, picks a weighted backend, and
// forwards the request via httputil.ReverseProxy.
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
		ErrorLog:  p.errorLog,
	}

	// Set or re-issue cookie (cookie-based persistence only).
	// Re-issue on every response when idleTimeout is configured to refresh lastActivity.
	if needsSessionCookie(route, sessionHit) {
		proxy.ModifyResponse = func(resp *http.Response) error {
			setSessionCookie(resp.Header, route, backend, sessionHit, sessionCreatedAt)
			return nil
		}
	}

	proxy.ServeHTTP(w, r)
}

// RoundTrip implements http.RoundTripper. It is called by cloudflared's origin
// proxy for WebSocket upgrades and gRPC requests, which require direct control
// over the HTTP round-trip (httputil.ReverseProxy's Hijack-based WebSocket
// handling doesn't work with cloudflared's streams, and gRPC needs HTTP/2
// framing with body flushing for streaming RPCs). It resolves the backend using
// the same routing logic as ServeHTTP and rewrites the request URL.
func (p *Proxy) RoundTrip(r *http.Request) (*http.Response, error) {
	route, backend, sessionHit, sessionCreatedAt, err := p.resolveBackend(r)
	if err != nil {
		return nil, err
	}

	r.URL.Scheme = backend.serviceURL.Scheme
	r.URL.Host = backend.serviceURL.Host
	resp, err := transportForRoute(route).RoundTrip(r)
	if err != nil {
		return nil, err
	}

	if needsSessionCookie(route, sessionHit) {
		setSessionCookie(resp.Header, route, backend, sessionHit, sessionCreatedAt)
	}
	return resp, nil
}

// ProtocolGRPC is the Protocol value for gRPC routes.
const ProtocolGRPC = "grpc"

// transportForRoute returns the appropriate HTTP transport for the route's protocol.
// gRPC routes use h2c (HTTP/2 cleartext); everything else uses HTTP/1.1.
func transportForRoute(route *Route) http.RoundTripper {
	if route.Protocol == ProtocolGRPC {
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

// needsSessionCookie reports whether a session cookie should be set or
// re-issued on the response.
func needsSessionCookie(route *Route, sessionHit bool) bool {
	sp := route.SessionPersistence
	return sp != nil && sp.Type == "Cookie" && (!sessionHit || sp.idleTimeout > 0)
}

// setSessionCookie adds a Set-Cookie header for session persistence.
func setSessionCookie(h http.Header, route *Route, backend *Backend, sessionHit bool, sessionCreatedAt int64) {
	sp := route.SessionPersistence
	createdAt := sessionCreatedAt
	if !sessionHit {
		createdAt = time.Now().Unix()
	}
	cookiePath := route.PathPrefix
	if cookiePath == "" {
		cookiePath = "/"
	}
	cookie := &http.Cookie{
		Name:     sp.SessionName,
		Value:    sessionCookieValue(backend.id, createdAt, sp.idleTimeout > 0),
		Path:     cookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if sp.CookieLifetimeType == "Permanent" {
		cookie.MaxAge = cookieMaxAge(sp, createdAt)
	}
	if v := cookie.String(); v != "" {
		h.Add("Set-Cookie", v)
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
// precomputed index, protocol match, then longest PathPrefix. gRPC requests
// only match routes with Protocol "grpc"; non-gRPC requests only match routes
// without a protocol set. Returns nil if no route matches.
func matchRoute(cfg *Config, host, path string, grpc bool) *Route {
	// Strip port from host if present.
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}

	routes := cfg.routesByHost[host]
	var best *Route
	bestLen := -1
	for _, r := range routes {
		if grpc != (r.Protocol == ProtocolGRPC) {
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
