// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: backend.URL, Weight: 1}}},
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
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: "http://backend:8080", Weight: 1}}},
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
			{Hostname: "app.example.com", PathPrefix: "/", Backends: []sidecar.Backend{{Service: rootBackend.URL, Weight: 1}}},
			{Hostname: "app.example.com", PathPrefix: "/api", Backends: []sidecar.Backend{{Service: apiBackend.URL, Weight: 1}}},
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
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: backend.URL, Weight: 1}}},
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
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: "http://valid:8080", Weight: 1}}},
			{Hostname: "bad.example.com", Backends: []sidecar.Backend{{Service: "://invalid", Weight: 1}}},
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
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: backend.URL, Weight: 1}}},
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
			{Hostname: "app.example.com", PathPrefix: "/api", Backends: []sidecar.Backend{{Service: backend.URL, Weight: 1}}},
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
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: backend.URL, Weight: 1}}},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com:8080/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))

	body, _ := io.ReadAll(rec.Body)
	g.Expect(string(body)).To(Equal("ok"))
}

func TestProxy_SingleBackend(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("single"))
	}))
	defer backend.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Backends: []sidecar.Backend{{Service: backend.URL, Weight: 1}}},
		},
	})

	for range 100 {
		req := httptest.NewRequest("GET", "http://app.example.com/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
		g.Expect(rec.Body.String()).To(Equal("single"))
	}
}

func TestProxy_WeightedBackends(t *testing.T) {
	g := NewWithT(t)

	var countA, countB atomic.Int32
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		countA.Add(1)
		_, _ = w.Write([]byte("a"))
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		countB.Add(1)
		_, _ = w.Write([]byte("b"))
	}))
	defer backendB.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Backends: []sidecar.Backend{
				{Service: backendA.URL, Weight: 80},
				{Service: backendB.URL, Weight: 20},
			}},
		},
	})

	const totalRequests = 1000
	for range totalRequests {
		req := httptest.NewRequest("GET", "http://app.example.com/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
	}

	a := int(countA.Load())
	b := int(countB.Load())
	g.Expect(a + b).To(Equal(totalRequests))
	// Both backends should receive traffic
	g.Expect(a).To(BeNumerically(">", 0))
	g.Expect(b).To(BeNumerically(">", 0))
	// Backend A should receive approximately 80% (±15% tolerance)
	g.Expect(a).To(BeNumerically(">=", 650)) // 80% - 15% = 65%
	g.Expect(a).To(BeNumerically("<=", 950)) // 80% + 15% = 95%
}

func TestProxy_ZeroWeightSkipped(t *testing.T) {
	g := NewWithT(t)

	var countA, countB atomic.Int32
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		countA.Add(1)
		_, _ = w.Write([]byte("a"))
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		countB.Add(1)
		_, _ = w.Write([]byte("b"))
	}))
	defer backendB.Close()

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Backends: []sidecar.Backend{
				{Service: backendA.URL, Weight: 0},
				{Service: backendB.URL, Weight: 1},
			}},
		},
	})

	for range 100 {
		req := httptest.NewRequest("GET", "http://app.example.com/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
		g.Expect(rec.Body.String()).To(Equal("b"))
	}

	g.Expect(int(countA.Load())).To(Equal(0))
	g.Expect(int(countB.Load())).To(Equal(100))
}

func TestProxy_AllZeroWeights502(t *testing.T) {
	g := NewWithT(t)

	p := &sidecar.Proxy{}
	setConfig(t, p, &sidecar.Config{
		Routes: []sidecar.Route{
			{Hostname: "app.example.com", Backends: []sidecar.Backend{
				{Service: "http://backend:8080", Weight: 0},
			}},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusBadGateway))
}
