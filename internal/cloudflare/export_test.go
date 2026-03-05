// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare

import (
	"io"
	"net/http"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/rs/zerolog"
)

// WithRetryMaxRetries creates a retryClient with the given maxRetries for testing.
func WithRetryMaxRetries(c Client, maxRetries int) Client {
	return &retryClient{inner: c, maxRetries: maxRetries}
}

// ParseTunnelToken exposes parseTunnelToken for testing.
var ParseTunnelToken = parseTunnelToken

// IsGRPC exposes isGRPC for testing.
var IsGRPC = isGRPC

// CloudflaredVersion exposes cloudflaredVersion for testing.
var CloudflaredVersion = cloudflaredVersion

// NewOriginProxy creates an originProxy for testing.
func NewOriginProxy(handler http.Handler, rt http.RoundTripper, log *zerolog.Logger) connection.OriginProxy {
	return &originProxy{handler: handler, rt: rt, log: log}
}

// NewBidirectionalStream creates a bidirectionalStream for testing.
func NewBidirectionalStream(reader io.Reader, writer io.Writer) io.ReadWriter {
	return &bidirectionalStream{reader: reader, writer: writer}
}

// NewOrchestratorWrapper creates an orchestratorWrapper for testing.
func NewOrchestratorWrapper(proxy connection.OriginProxy) *orchestratorWrapper {
	return &orchestratorWrapper{proxy: proxy}
}
