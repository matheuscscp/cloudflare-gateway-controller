// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"
)

// Config is the top-level sidecar proxy configuration, serialized as YAML
// in the per-Gateway ConfigMap.
type Config struct {
	Routes []Route `json:"routes"`

	routesByHost map[string][]*Route // hostname -> routes index, populated by Config.Parse
}

// Parse validates and parses every backend's Service URL, computes backend IDs,
// and parses session persistence timeouts. It must be called before the Config
// is used by the Proxy. Returns an error if any URL is invalid.
func (c *Config) Parse() error {
	for i := range c.Routes {
		var totalWeight int32
		for j := range c.Routes[i].Backends {
			b := &c.Routes[i].Backends[j]
			u, err := url.Parse(b.Service)
			if err != nil {
				return fmt.Errorf("route %d (%s) backend %d: invalid service URL %q: %w",
					i, c.Routes[i].Hostname, j, b.Service, err)
			}
			b.serviceURL = u
			h := sha256.Sum256([]byte(b.Service))
			b.id = hex.EncodeToString(h[:])[:16]
			totalWeight += b.Weight
		}
		c.Routes[i].totalWeight = totalWeight
		if sp := c.Routes[i].SessionPersistence; sp != nil {
			if sp.AbsoluteTimeout != "" {
				d, err := time.ParseDuration(sp.AbsoluteTimeout)
				if err != nil {
					return fmt.Errorf("route %d (%s): invalid absoluteTimeout %q: %w",
						i, c.Routes[i].Hostname, sp.AbsoluteTimeout, err)
				}
				sp.absoluteTimeout = d
			}
			if sp.IdleTimeout != "" {
				d, err := time.ParseDuration(sp.IdleTimeout)
				if err != nil {
					return fmt.Errorf("route %d (%s): invalid idleTimeout %q: %w",
						i, c.Routes[i].Hostname, sp.IdleTimeout, err)
				}
				sp.idleTimeout = d
			}
		}
	}

	// Build hostname index for O(1) lookup in matchRoute.
	c.routesByHost = make(map[string][]*Route, len(c.Routes))
	for i := range c.Routes {
		h := c.Routes[i].Hostname
		c.routesByHost[h] = append(c.routesByHost[h], &c.Routes[i])
	}

	return nil
}

// SessionPersistence configures session affinity for a route.
type SessionPersistence struct {
	Type               string `json:"type"`                         // "Cookie" or "Header"
	SessionName        string `json:"sessionName"`                  // cookie/header name
	AbsoluteTimeout    string `json:"absoluteTimeout,omitempty"`    // Go duration string
	IdleTimeout        string `json:"idleTimeout,omitempty"`        // Go duration string
	CookieLifetimeType string `json:"cookieLifetimeType,omitempty"` // "Session" or "Permanent"

	absoluteTimeout time.Duration
	idleTimeout     time.Duration
}

// Backend maps a service URL and weight for weighted traffic splitting.
type Backend struct {
	Service string `json:"service"` // e.g. "http://my-svc.ns.svc.cluster.local:8080"
	Weight  int32  `json:"weight"`  // default 1, range 0–1,000,000

	serviceURL *url.URL // pre-parsed Service URL, populated by Config.Parse
	id         string   // hex(sha256(Service))[:16], populated by Config.Parse
}

// Route maps a hostname + optional path prefix to one or more weighted backend
// service URLs.
type Route struct {
	Hostname           string              `json:"hostname"`             // e.g. "app.example.com"
	PathPrefix         string              `json:"pathPrefix,omitempty"` // e.g. "/api" (match only, forwarded as-is)
	Owner              string              `json:"owner,omitempty"`      // "namespace/name" of the owning HTTPRoute
	Backends           []Backend           `json:"backends"`
	SessionPersistence *SessionPersistence `json:"sessionPersistence,omitempty"`

	totalWeight int32 // sum of backend weights, populated by Config.Parse
}
