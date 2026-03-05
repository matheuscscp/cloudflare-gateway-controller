// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/tracing"
	. "github.com/onsi/gomega"
	"github.com/rs/zerolog"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

func TestParseTunnelToken(t *testing.T) {
	t.Run("valid token", func(t *testing.T) {
		g := NewWithT(t)
		tokenJSON := `{"a":"account-tag","s":"c2VjcmV0","t":"01234567-89ab-cdef-0123-456789abcdef"}`
		encoded := base64.StdEncoding.EncodeToString([]byte(tokenJSON))
		token, err := cloudflare.ParseTunnelToken(encoded)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(token).NotTo(BeNil())
	})

	t.Run("invalid base64", func(t *testing.T) {
		g := NewWithT(t)
		_, err := cloudflare.ParseTunnelToken("not-valid-base64!!!")
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("invalid JSON", func(t *testing.T) {
		g := NewWithT(t)
		encoded := base64.StdEncoding.EncodeToString([]byte("not json"))
		_, err := cloudflare.ParseTunnelToken(encoded)
		g.Expect(err).To(HaveOccurred())
	})
}

func TestIsGRPC(t *testing.T) {
	g := NewWithT(t)

	grpcReq, _ := http.NewRequest("POST", "http://example.com/service/Method", nil)
	grpcReq.Header.Set("Content-Type", "application/grpc")
	g.Expect(cloudflare.IsGRPC(grpcReq)).To(BeTrue())

	grpcReqPlus, _ := http.NewRequest("POST", "http://example.com/service/Method", nil)
	grpcReqPlus.Header.Set("Content-Type", "application/grpc+proto")
	g.Expect(cloudflare.IsGRPC(grpcReqPlus)).To(BeTrue())

	htmlReq, _ := http.NewRequest("GET", "http://example.com/", nil)
	htmlReq.Header.Set("Content-Type", "text/html")
	g.Expect(cloudflare.IsGRPC(htmlReq)).To(BeFalse())

	noContentType, _ := http.NewRequest("GET", "http://example.com/", nil)
	g.Expect(cloudflare.IsGRPC(noContentType)).To(BeFalse())
}

func TestCloudflaredVersion(t *testing.T) {
	g := NewWithT(t)
	version := cloudflare.CloudflaredVersion()
	// In test builds, this returns the module version or "DEV".
	g.Expect(version).NotTo(BeEmpty())
}

func TestBidirectionalStream(t *testing.T) {
	t.Run("Read", func(t *testing.T) {
		g := NewWithT(t)
		reader := strings.NewReader("hello")
		stream := cloudflare.NewBidirectionalStream(reader, io.Discard)
		buf := make([]byte, 5)
		n, err := stream.Read(buf)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n).To(Equal(5))
		g.Expect(string(buf)).To(Equal("hello"))
	})

	t.Run("Write", func(t *testing.T) {
		g := NewWithT(t)
		var buf bytes.Buffer
		stream := cloudflare.NewBidirectionalStream(strings.NewReader(""), &buf)
		n, err := stream.Write([]byte("world"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n).To(Equal(5))
		g.Expect(buf.String()).To(Equal("world"))
	})
}

func TestGetOriginProxy(t *testing.T) {
	g := NewWithT(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(handler, http.DefaultTransport, &zlog)
	wrapper := cloudflare.NewOrchestratorWrapper(proxy)
	got, err := wrapper.GetOriginProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(proxy))
}

func TestProxyTCP(t *testing.T) {
	g := NewWithT(t)
	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), http.DefaultTransport, &zlog)
	err := proxy.ProxyTCP(context.Background(), nil, &connection.TCPRequest{})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("TCP proxying not yet supported"))
}

// mockResponseWriter implements connection.ResponseWriter for testing.
type mockResponseWriter struct {
	statusCode int
	headers    http.Header
	body       bytes.Buffer
	trailers   map[string][]string
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{
		headers:  make(http.Header),
		trailers: make(map[string][]string),
	}
}

func (m *mockResponseWriter) WriteRespHeaders(status int, header http.Header) error {
	m.statusCode = status
	maps.Copy(m.headers, header)
	return nil
}

func (m *mockResponseWriter) AddTrailer(name, value string) {
	m.trailers[name] = append(m.trailers[name], value)
}

func (m *mockResponseWriter) Header() http.Header         { return m.headers }
func (m *mockResponseWriter) WriteHeader(statusCode int)  { m.statusCode = statusCode }
func (m *mockResponseWriter) Write(b []byte) (int, error) { return m.body.Write(b) }
func (m *mockResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

func TestProxyHTTP_RegularRequest(t *testing.T) {
	g := NewWithT(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from origin"))
	})
	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(handler, http.DefaultTransport, &zlog)

	req, _ := http.NewRequest("GET", "http://example.com/path", nil)
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rw.body.String()).To(Equal("hello from origin"))
}

func TestProxyHTTP_GRPCRequest(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("grpc-response-body"))
	}))
	defer backend.Close()

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), backend.Client().Transport, &zlog)

	req, _ := http.NewRequest("POST", backend.URL+"/service/Method", nil)
	req.Header.Set("Content-Type", "application/grpc")
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rw.statusCode).To(Equal(http.StatusOK))
	g.Expect(rw.body.String()).To(Equal("grpc-response-body"))
}

func TestProxyHTTP_WebSocket(t *testing.T) {
	g := NewWithT(t)

	// Backend returns a non-101 response (simulates WebSocket rejection).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer backend.Close()

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), backend.Client().Transport, &zlog)

	req, _ := http.NewRequest("GET", backend.URL+"/ws", nil)
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rw.statusCode).To(Equal(http.StatusForbidden))
	g.Expect(rw.body.String()).To(Equal("forbidden"))
}

func TestProxyHTTP_WebSocket_RoundTripError(t *testing.T) {
	g := NewWithT(t)

	// Use a transport that always fails.
	failTransport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), failTransport, &zlog)

	req, _ := http.NewRequest("GET", "http://example.com/ws", nil)
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, true)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("websocket roundtrip"))
}

func TestProxyHTTP_GRPC_RoundTripError(t *testing.T) {
	g := NewWithT(t)

	failTransport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), failTransport, &zlog)

	req, _ := http.NewRequest("POST", "http://example.com/service/Method", nil)
	req.Header.Set("Content-Type", "application/grpc")
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("grpc roundtrip"))
}

func TestProxyHTTP_GRPC_WithTrailers(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "grpc-status, grpc-message")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data"))
		w.Header().Set(http.TrailerPrefix+"grpc-status", "0")
		w.Header().Set(http.TrailerPrefix+"grpc-message", "OK")
	}))
	defer backend.Close()

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), backend.Client().Transport, &zlog)

	req, _ := http.NewRequest("POST", backend.URL+"/service/Method", nil)
	req.Header.Set("Content-Type", "application/grpc")
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rw.body.String()).To(Equal("data"))
}

func TestProxyHTTP_WebSocket_WriteRespHeadersError(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), backend.Client().Transport, &zlog)

	req, _ := http.NewRequest("GET", backend.URL+"/ws", nil)
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := &errResponseWriter{writeRespHeadersErr: io.ErrClosedPipe}

	err := proxy.ProxyHTTP(rw, tracedReq, true)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("writing websocket response headers"))
}

func TestProxyHTTP_WebSocket_BodyNotReadWriteCloser(t *testing.T) {
	g := NewWithT(t)

	// Return 101 with a body that is NOT a ReadWriteCloser (just a ReadCloser).
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), rt, &zlog)

	req, _ := http.NewRequest("GET", "http://example.com/ws", strings.NewReader(""))
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newMockResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, true)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ReadWriteCloser"))
}

func TestProxyHTTP_GRPC_WriteRespHeadersError(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	zlog := zerolog.Nop()
	proxy := cloudflare.NewOriginProxy(http.NotFoundHandler(), backend.Client().Transport, &zlog)

	req, _ := http.NewRequest("POST", backend.URL+"/service/Method", nil)
	req.Header.Set("Content-Type", "application/grpc")
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := &errResponseWriter{writeRespHeadersErr: io.ErrClosedPipe}

	err := proxy.ProxyHTTP(rw, tracedReq, false)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("writing grpc response headers"))
}

// errResponseWriter is a connection.ResponseWriter that returns errors on WriteRespHeaders.
type errResponseWriter struct {
	writeRespHeadersErr error
	body                bytes.Buffer
	headers             http.Header
}

func (e *errResponseWriter) WriteRespHeaders(int, http.Header) error { return e.writeRespHeadersErr }
func (e *errResponseWriter) AddTrailer(name, value string)           {}
func (e *errResponseWriter) Header() http.Header {
	if e.headers == nil {
		e.headers = make(http.Header)
	}
	return e.headers
}
func (e *errResponseWriter) WriteHeader(int)             {}
func (e *errResponseWriter) Write(b []byte) (int, error) { return e.body.Write(b) }
func (e *errResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

// roundTripFunc implements http.RoundTripper via a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
