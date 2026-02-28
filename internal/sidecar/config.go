// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar

import (
	"fmt"
	"net/url"
)

// Config is the top-level sidecar proxy configuration, serialized as YAML
// in the per-Gateway ConfigMap.
type Config struct {
	Routes []Route `json:"routes"`
}

// ParseServiceURLs validates and parses every backend's Service URL. It must be
// called before the Config is used by the Proxy. Returns an error if any URL
// is invalid.
func (c *Config) ParseServiceURLs() error {
	for i := range c.Routes {
		for j := range c.Routes[i].Backends {
			u, err := url.Parse(c.Routes[i].Backends[j].Service)
			if err != nil {
				return fmt.Errorf("route %d (%s) backend %d: invalid service URL %q: %w",
					i, c.Routes[i].Hostname, j, c.Routes[i].Backends[j].Service, err)
			}
			c.Routes[i].Backends[j].serviceURL = u
		}
	}
	return nil
}

// Backend maps a service URL and weight for weighted traffic splitting.
type Backend struct {
	Service string `json:"service"` // e.g. "http://my-svc.ns.svc.cluster.local:8080"
	Weight  int32  `json:"weight"`  // default 1, range 0–1,000,000

	serviceURL *url.URL // pre-parsed Service URL, populated by Config.ParseServiceURLs
}

// Route maps a hostname + optional path prefix to one or more weighted backend
// service URLs.
type Route struct {
	Hostname   string    `json:"hostname"`             // e.g. "app.example.com"
	PathPrefix string    `json:"pathPrefix,omitempty"` // e.g. "/api" (match only, forwarded as-is)
	Backends   []Backend `json:"backends"`
}
