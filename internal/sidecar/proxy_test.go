// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/sidecar"
)

func setConfig(t *testing.T, p *sidecar.Proxy, cfg *sidecar.Config) {
	t.Helper()
	g := NewWithT(t)
	g.Expect(cfg.ParseServiceURLs()).To(Succeed())
	p.SetConfig(cfg)
}

func TestProxy_HostnameRouting(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Service: backend.URL},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Body.String()).To(Equal("ok"))
}

func TestProxy_NoMatch404(t *testing.T) {
	g := NewWithT(t)

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Service: "http://backend:8080"},
		},
	})

	req := httptest.NewRequest("GET", "http://unknown.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusNotFound))
}

func TestProxy_LongestPathPrefixMatch(t *testing.T) {
	g := NewWithT(t)

	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api"))
	}))
	defer apiBackend.Close()

	rootBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("root"))
	}))
	defer rootBackend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", PathPrefix: "/", Service: rootBackend.URL},
			{Hostname: "app.example.com", PathPrefix: "/api", Service: apiBackend.URL},
		},
	})

	// /api/v1 should match the /api route (longest prefix)
	req := httptest.NewRequest("GET", "http://app.example.com/api/v1", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Body.String()).To(Equal("api"))

	// / should match the / route
	req = httptest.NewRequest("GET", "http://app.example.com/other", nil)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Body.String()).To(Equal("root"))
}

func TestProxy_NoConfig503(t *testing.T) {
	g := NewWithT(t)

	p := &sidecar.Proxy{}

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
	g.Expect(rec.Header().Get(sidecar.HeaderConfigNotLoaded)).To(Equal("true"))
}

func TestProxy_HostHeaderForwarded(t *testing.T) {
	g := NewWithT(t)

	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Service: backend.URL},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	// The original Host header should be forwarded, not replaced by the backend host.
	g.Expect(receivedHost).To(Equal("app.example.com"))
}

func TestParseServiceURLs_InvalidURL(t *testing.T) {
	g := NewWithT(t)

	cfg := &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Service: "http://valid:8080"},
			{Hostname: "bad.example.com", Service: "://invalid"},
		},
	}
	err := cfg.ParseServiceURLs()
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("bad.example.com"))
}

func TestProxy_DisableKeepAlives(t *testing.T) {
	g := NewWithT(t)

	var sawConnection string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawConnection = r.Header.Get("Connection")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Service: backend.URL},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	// When DisableKeepAlives is true, the transport sets Connection: close
	g.Expect(sawConnection).To(Equal("close"))
}

func TestProxy_PathForwardedAsIs(t *testing.T) {
	g := NewWithT(t)

	var receivedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", PathPrefix: "/api", Service: backend.URL},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/api/v1/users", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	// Path is forwarded as-is, not stripped
	g.Expect(receivedPath).To(Equal("/api/v1/users"))
}

func TestProxy_HostWithPort(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Service: backend.URL},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com:8080/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))

	body, _ := io.ReadAll(rec.Body)
	g.Expect(string(body)).To(Equal("ok"))
}
