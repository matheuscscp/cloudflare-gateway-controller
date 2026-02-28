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

// ParseServiceURLs validates and parses every route's Service URL. It must be
// called before the Config is used by the Proxy. Returns an error if any URL
// is invalid.
func (c *Config) ParseServiceURLs() error {
	for i := range c.Routes {
		u, err := url.Parse(c.Routes[i].Service)
		if err != nil {
			return fmt.Errorf("route %d (%s): invalid service URL %q: %w", i, c.Routes[i].Hostname, c.Routes[i].Service, err)
		}
		c.Routes[i].serviceURL = u
	}
	return nil
}

// Route maps a hostname + optional path prefix to a backend service URL.
type Route struct {
	Hostname   string `json:"hostname"`             // e.g. "app.example.com"
	PathPrefix string `json:"pathPrefix,omitempty"` // e.g. "/api" (match only, forwarded as-is)
	Service    string `json:"service"`              // e.g. "http://my-svc.ns.svc.cluster.local:8080"

	serviceURL *url.URL // pre-parsed Service URL, populated by Config.ParseServiceURLs
}
