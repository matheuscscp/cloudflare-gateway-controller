// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package proxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/proxy"
)

func setConfig(t *testing.T, p *proxy.Proxy, cfg *proxy.Config) {
	t.Helper()
	g := NewWithT(t)
	g.Expect(cfg.Parse()).To(Succeed())
	p.SetConfig(cfg)
}

func TestProxy_HostnameRouting(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}}},
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: "http://backend:8080", Weight: 1}}},
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", PathPrefix: "/", Backends: []proxy.Backend{{Service: rootBackend.URL, Weight: 1}}},
			{Hostname: "app.example.com", PathPrefix: "/api", Backends: []proxy.Backend{{Service: apiBackend.URL, Weight: 1}}},
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

	p := &proxy.Proxy{}

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
	g.Expect(rec.Header().Get(proxy.HeaderConfigNotLoaded)).To(Equal("true"))
}

func TestProxy_HostHeaderForwarded(t *testing.T) {
	g := NewWithT(t)

	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}}},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	// The original Host header should be forwarded, not replaced by the backend host.
	g.Expect(receivedHost).To(Equal("app.example.com"))
}

func TestParse_InvalidURL(t *testing.T) {
	g := NewWithT(t)

	cfg := &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: "http://valid:8080", Weight: 1}}},
			{Hostname: "bad.example.com", Backends: []proxy.Backend{{Service: "://invalid", Weight: 1}}},
		},
	}
	err := cfg.Parse()
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("bad.example.com"))
}

func TestParse_InvalidAbsoluteTimeout(t *testing.T) {
	g := NewWithT(t)

	cfg := &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: "http://valid:8080", Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:            "Cookie",
					SessionName:     "test",
					AbsoluteTimeout: "not-a-duration",
				},
			},
		},
	}
	err := cfg.Parse()
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("absoluteTimeout"))
}

func TestParse_InvalidIdleTimeout(t *testing.T) {
	g := NewWithT(t)

	cfg := &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: "http://valid:8080", Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:        "Cookie",
					SessionName: "test",
					IdleTimeout: "invalid",
				},
			},
		},
	}
	err := cfg.Parse()
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("idleTimeout"))
}

func TestProxy_DisableKeepAlives(t *testing.T) {
	g := NewWithT(t)

	var sawConnection string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawConnection = r.Header.Get("Connection")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}}},
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", PathPrefix: "/api", Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}}},
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}}},
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}}},
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{Hostname: "app.example.com", Backends: []proxy.Backend{
				{Service: "http://backend:8080", Weight: 0},
			}},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusBadGateway))
}

func TestProxy_CookieSessionPersistence(t *testing.T) {
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

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{
					{Service: backendA.URL, Weight: 50},
					{Service: backendB.URL, Weight: 50},
				},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// First request — no cookie, should get Set-Cookie.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	firstBody := rec.Body.String()

	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	g.Expect(cookies[0].Name).To(Equal("cgw-session"))
	g.Expect(cookies[0].Path).To(Equal("/"))
	g.Expect(cookies[0].HttpOnly).To(BeTrue())

	// Follow-up requests with cookie — all should go to the same backend.
	for range 100 {
		req = httptest.NewRequest("GET", "http://app.example.com/", nil)
		req.AddCookie(cookies[0])
		rec = httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
		g.Expect(rec.Body.String()).To(Equal(firstBody))
		// No new Set-Cookie on session hit.
		g.Expect(rec.Result().Cookies()).To(BeEmpty())
	}
}

func TestProxy_CookieSessionPersistence_BackendRemoved(t *testing.T) {
	g := NewWithT(t)

	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a"))
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("b"))
	}))
	defer backendB.Close()

	p := &proxy.Proxy{}
	sp := &proxy.SessionPersistence{
		Type:               "Cookie",
		SessionName:        "cgw-session",
		CookieLifetimeType: "Session",
	}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname:           "app.example.com",
				Backends:           []proxy.Backend{{Service: backendA.URL, Weight: 50}, {Service: backendB.URL, Weight: 50}},
				SessionPersistence: sp,
			},
		},
	})

	// First request to get pinned.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	pinnedBody := rec.Body.String()

	// Swap config: remove the pinned backend, keep only the other.
	var remainingURL string
	if pinnedBody == "a" {
		remainingURL = backendB.URL
	} else {
		remainingURL = backendA.URL
	}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname:           "app.example.com",
				Backends:           []proxy.Backend{{Service: remainingURL, Weight: 1}},
				SessionPersistence: sp,
			},
		},
	})

	// Request with old cookie → backend not found → falls back to weighted random, new cookie.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Body.String()).NotTo(Equal(pinnedBody))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New cookie issued.
}

func TestProxy_CookieSessionPersistence_PermanentCookie(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}

	// Permanent cookie with AbsoluteTimeout should have Max-Age.
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					AbsoluteTimeout:    "1h",
					CookieLifetimeType: "Permanent",
				},
			},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	g.Expect(cookies[0].MaxAge).To(Equal(3600))

	// Session cookie should NOT have Max-Age.
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies = rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	g.Expect(cookies[0].MaxAge).To(Equal(0))
}

func TestProxy_CookieSessionPersistence_AbsoluteTimeout(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					AbsoluteTimeout:    "1s",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// Get initial cookie.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))

	// Forge cookie with old timestamp (expired).
	expiredCookie := &http.Cookie{
		Name:  "cgw-session",
		Value: fmt.Sprintf("%s.%d", cookies[0].Value[:16], time.Now().Unix()-10),
	}

	// Request with expired cookie → session expired → new cookie issued.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(expiredCookie)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New cookie issued.
}

func TestProxy_CookieSessionPersistence_IdleTimeout(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					IdleTimeout:        "1s",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// First request — new session, cookie should be 3-segment (backendID.createdAt.lastActivity).
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))

	// Verify 3-segment format.
	parts := splitCookieValue(cookies[0].Value)
	g.Expect(parts).To(HaveLen(3))

	// Session hit should re-issue cookie (because idleTimeout is set).
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // Re-issued!

	// Forge cookie with old lastActivity → idle expired.
	backendID := parts[0]
	createdAt := parts[1]
	expiredCookie := &http.Cookie{
		Name:  "cgw-session",
		Value: fmt.Sprintf("%s.%s.%d", backendID, createdAt, time.Now().Unix()-10),
	}
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(expiredCookie)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New session cookie.
}

func TestProxy_CookieSessionPersistence_BothTimeouts(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					AbsoluteTimeout:    "1h",
					IdleTimeout:        "10m",
					CookieLifetimeType: "Permanent",
				},
			},
		},
	})

	// First request — new session.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))

	// Max-Age should be min(remainingAbsolute=3600, idle=600) = 600.
	g.Expect(cookies[0].MaxAge).To(Equal(600))

	// Forge cookie with createdAt 50 minutes ago (remaining absolute = 10min = 600s).
	// Min(600, 600) = 600.
	parts := splitCookieValue(cookies[0].Value)
	g.Expect(parts).To(HaveLen(3))
	backendID := parts[0]
	forgedCookie := &http.Cookie{
		Name:  "cgw-session",
		Value: fmt.Sprintf("%s.%d.%d", backendID, time.Now().Unix()-3000, time.Now().Unix()),
	}
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(forgedCookie)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	reissuedCookies := rec.Result().Cookies()
	g.Expect(reissuedCookies).To(HaveLen(1))
	// Remaining absolute ≈ 3600 - 3000 = 600, idle = 600, min = 600.
	g.Expect(reissuedCookies[0].MaxAge).To(Equal(600))

	// Forge cookie with old createdAt → absolute timeout expired.
	expiredAbsCookie := &http.Cookie{
		Name:  "cgw-session",
		Value: fmt.Sprintf("%s.%d.%d", backendID, time.Now().Unix()-3700, time.Now().Unix()),
	}
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(expiredAbsCookie)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New session (expired).

	// Forge cookie with recent createdAt but old lastActivity → idle timeout expired.
	expiredIdleCookie := &http.Cookie{
		Name:  "cgw-session",
		Value: fmt.Sprintf("%s.%d.%d", backendID, time.Now().Unix(), time.Now().Unix()-700),
	}
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(expiredIdleCookie)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New session (idle expired).
}

func TestProxy_CookieSessionPersistence_IdleTimeoutReissue(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					IdleTimeout:        "10m",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// Get initial cookie.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))

	originalParts := splitCookieValue(cookies[0].Value)
	g.Expect(originalParts).To(HaveLen(3))
	originalCreatedAt := originalParts[1]

	// Re-issue: send request with the cookie.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	reissued := rec.Result().Cookies()
	g.Expect(reissued).To(HaveLen(1))

	reissuedParts := splitCookieValue(reissued[0].Value)
	g.Expect(reissuedParts).To(HaveLen(3))

	// CreatedAt should be preserved.
	g.Expect(reissuedParts[1]).To(Equal(originalCreatedAt))

	// BackendID should be preserved.
	g.Expect(reissuedParts[0]).To(Equal(originalParts[0]))
}

// splitCookieValue splits a session cookie value by "." for test assertions.
func splitCookieValue(value string) []string {
	return strings.Split(value, ".")
}

func TestProxy_SessionPersistence_MalformedCookieValues(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// Malformed 2-part cookie: non-numeric timestamp.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "backendid.notanumber"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // Falls back to new cookie.

	// Malformed 3-part cookie: non-numeric createdAt.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "backendid.bad.123"})
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1))

	// Malformed 3-part cookie: non-numeric lastActivity.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "backendid.123.bad"})
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1))

	// Too many segments (non-numeric timestamps, fails at createdAt parse).
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "a.b.c.d"})
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1))

	// Too many segments (numeric timestamps, extra dot rejected).
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "backend.1.2.3"})
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1))
}

func TestProxy_CookieSessionPersistence_PermanentIdleTimeoutOnly(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					IdleTimeout:        "5m",
					CookieLifetimeType: "Permanent",
				},
			},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	g.Expect(cookies[0].MaxAge).To(Equal(300)) // 5 minutes = 300 seconds
}

func TestProxy_CookieSessionPersistence_PermanentNoTimeouts(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					CookieLifetimeType: "Permanent",
				},
			},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	g.Expect(cookies[0].MaxAge).To(Equal(0)) // No timeouts → MaxAge 0.
}

func TestProxy_CookieSessionPersistence_BothTimeoutsRemainingSmaller(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					AbsoluteTimeout:    "1h",
					IdleTimeout:        "10m",
					CookieLifetimeType: "Permanent",
				},
			},
		},
	})

	// Get initial cookie.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))

	parts := splitCookieValue(cookies[0].Value)
	g.Expect(parts).To(HaveLen(3))
	backendID := parts[0]

	// Forge cookie with createdAt 55 minutes ago (remaining absolute = 5min = 300s < idle = 600s).
	forgedCookie := &http.Cookie{
		Name:  "cgw-session",
		Value: fmt.Sprintf("%s.%d.%d", backendID, time.Now().Unix()-3300, time.Now().Unix()),
	}
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(forgedCookie)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	reissuedCookies := rec.Result().Cookies()
	g.Expect(reissuedCookies).To(HaveLen(1))
	// Remaining absolute ≈ 3600 - 3300 = 300 < idle 600, so Max-Age = 300.
	g.Expect(reissuedCookies[0].MaxAge).To(Equal(300))
}

func TestProxy_HeaderSessionPersistence_UnknownBackend(t *testing.T) {
	g := NewWithT(t)

	var countA atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		countA.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:        "Header",
					SessionName: "X-Session",
				},
			},
		},
	})

	// Unknown backend ID in header → falls back to weighted random.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.Header.Set("X-Session", "unknown-backend-id")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(int(countA.Load())).To(Equal(1))
	// Header-based: no Set-Cookie issued.
	g.Expect(rec.Result().Cookies()).To(BeEmpty())
}

func TestProxy_HeaderSessionPersistence(t *testing.T) {
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

	p := &proxy.Proxy{}
	cfg := &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{
					{Service: backendA.URL, Weight: 50},
					{Service: backendB.URL, Weight: 50},
				},
				SessionPersistence: &proxy.SessionPersistence{
					Type:        "Header",
					SessionName: "X-Session",
				},
			},
		},
	}
	setConfig(t, p, cfg)

	// Request without header — weighted random, no Set-Cookie.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(BeEmpty())
	firstBody := rec.Body.String()

	// Get the backend ID for pinning. We need to figure out which backend
	// was selected and use its ID.
	g.Expect(cfg.Parse()).To(Succeed())
	var backendID string
	if firstBody == "a" {
		backendID = cfg.Routes[0].Backends[0].Service // need to get the ID
	}
	// Actually let's re-parse and get the ID directly.
	_ = backendID

	// We'll do it the simple way: send requests with each backend's known ID.
	// Parse config to compute IDs.
	parsedCfg := &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{
					{Service: backendA.URL, Weight: 50},
					{Service: backendB.URL, Weight: 50},
				},
				SessionPersistence: &proxy.SessionPersistence{
					Type:        "Header",
					SessionName: "X-Session",
				},
			},
		},
	}
	g.Expect(parsedCfg.Parse()).To(Succeed())

	// Reset counts.
	countA.Store(0)
	countB.Store(0)

	// Send 100 requests pinned to backend A via header.
	// We need to discover the backend ID. Send a request without header,
	// check which backend responded, find its ID from the config.
	// Since we can't export the ID field, let's use a known pattern:
	// just send requests with header and verify affinity works.
	// Instead, let's use the first request's body to determine which backend
	// was hit, then use the Config's computed IDs.
	// But Backend.id is unexported... We need another approach.
	// Let's use the cookie-based test as reference: first request determines
	// the pinned backend, then we verify all subsequent go to the same one.

	// For header-based, the client must supply the header value (the backend ID).
	// Since the ID is unexported, we'll test via the pattern: first request
	// without header → random, then all requests with the SAME header value
	// should go to the same backend.

	// Actually, since the ID is internal, the test needs to discover it.
	// We can do this by getting the Set-Cookie from a cookie-based config
	// on the same backend, OR we can test with "unknown header" → falls back.

	// Simplest approach: just test that without header, traffic is random,
	// and with a wrong header, it also falls back to random (tested separately).
	// For real header persistence testing, use the cookie value approach.

	// Let me take a different approach: configure cookie-based first to discover
	// the backend ID, then switch to header-based.

	// Actually, let's just send many requests and verify that all go to the
	// same backend when we send with the header that was "discovered".
	// We'll use the cookie response from a temporary cookie config to get the ID.

	// Set cookie config temporarily.
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{
					{Service: backendA.URL, Weight: 50},
					{Service: backendB.URL, Weight: 50},
				},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "discover",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// Discover backend A's ID.
	var backendAID string
	for range 100 {
		req = httptest.NewRequest("GET", "http://app.example.com/", nil)
		rec = httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Body.String() == "a" {
			cookieVal := rec.Result().Cookies()[0].Value
			// Cookie value is "backendID.timestamp"
			dotIdx := len(cookieVal) - 1
			for dotIdx >= 0 && cookieVal[dotIdx] != '.' {
				dotIdx--
			}
			backendAID = cookieVal[:dotIdx]
			break
		}
	}
	g.Expect(backendAID).NotTo(BeEmpty())

	// Switch back to header-based config.
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{
					{Service: backendA.URL, Weight: 50},
					{Service: backendB.URL, Weight: 50},
				},
				SessionPersistence: &proxy.SessionPersistence{
					Type:        "Header",
					SessionName: "X-Session",
				},
			},
		},
	})

	countA.Store(0)
	countB.Store(0)

	// All requests with backend A's ID should go to backend A.
	for range 100 {
		req = httptest.NewRequest("GET", "http://app.example.com/", nil)
		req.Header.Set("X-Session", backendAID)
		rec = httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
		g.Expect(rec.Body.String()).To(Equal("a"))
		// No Set-Cookie on any response (header-based).
		g.Expect(rec.Result().Cookies()).To(BeEmpty())
	}

	g.Expect(int(countA.Load())).To(Equal(100))
	g.Expect(int(countB.Load())).To(Equal(0))
}

func TestProxy_SessionPersistence_InvalidToken(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	// Forged/garbage cookie → falls back to weighted random, new cookie set.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "garbage-value"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New cookie issued.

	// Another garbage: valid format but unknown backend ID.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "cgw-session", Value: "0000000000000000.1709312400"})
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New cookie issued.
}

func TestProxy_SessionPersistence_ZeroWeightBackend(t *testing.T) {
	g := NewWithT(t)

	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a"))
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("b"))
	}))
	defer backendB.Close()

	sp := &proxy.SessionPersistence{
		Type:               "Cookie",
		SessionName:        "cgw-session",
		CookieLifetimeType: "Session",
	}

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname:           "app.example.com",
				Backends:           []proxy.Backend{{Service: backendA.URL, Weight: 50}, {Service: backendB.URL, Weight: 50}},
				SessionPersistence: sp,
			},
		},
	})

	// Get pinned to one backend.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	pinnedBody := rec.Body.String()

	// Set pinned backend to weight 0 in config reload.
	var backends []proxy.Backend
	if pinnedBody == "a" {
		backends = []proxy.Backend{{Service: backendA.URL, Weight: 0}, {Service: backendB.URL, Weight: 50}}
	} else {
		backends = []proxy.Backend{{Service: backendA.URL, Weight: 50}, {Service: backendB.URL, Weight: 0}}
	}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname:           "app.example.com",
				Backends:           backends,
				SessionPersistence: sp,
			},
		},
	})

	// Request with old cookie → pinned backend has weight 0 → session invalidated, new cookie.
	req = httptest.NewRequest("GET", "http://app.example.com/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Body.String()).NotTo(Equal(pinnedBody))
	g.Expect(rec.Result().Cookies()).To(HaveLen(1)) // New cookie issued.
}

func TestProxy_SessionPersistence_NoPersistenceField(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname: "app.example.com",
				Backends: []proxy.Backend{{Service: backend.URL, Weight: 1}},
				// No SessionPersistence
			},
		},
	})

	for range 10 {
		req := httptest.NewRequest("GET", "http://app.example.com/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
		g.Expect(rec.Body.String()).To(Equal("ok"))
		// No cookies.
		g.Expect(rec.Result().Cookies()).To(BeEmpty())
	}
}

func TestProxy_CookieSessionPersistence_PathScope(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := &proxy.Proxy{}
	setConfig(t, p, &proxy.Config{
		Routes: []proxy.Route{
			{
				Hostname:   "app.example.com",
				PathPrefix: "/api",
				Backends:   []proxy.Backend{{Service: backend.URL, Weight: 1}},
				SessionPersistence: &proxy.SessionPersistence{
					Type:               "Cookie",
					SessionName:        "cgw-session",
					CookieLifetimeType: "Session",
				},
			},
		},
	})

	req := httptest.NewRequest("GET", "http://app.example.com/api/v1", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	cookies := rec.Result().Cookies()
	g.Expect(cookies).To(HaveLen(1))
	g.Expect(cookies[0].Path).To(Equal("/api"))
}
