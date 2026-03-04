// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
)

const defaultMaxRetries = 3

// WithRetry wraps a Client with retry logic for transient network errors
// such as unexpected EOF during response body reads and connection resets.
// These errors are not retried by the cloudflare-go SDK because they occur
// after the HTTP response status is received (during body parsing).
func WithRetry(c Client) Client {
	return &retryClient{inner: c, maxRetries: defaultMaxRetries}
}

type retryClient struct {
	inner      Client
	maxRetries int
}

// IsTransient reports whether an error is a transient network error that
// should be retried. API errors (4xx/5xx with structured bodies) are NOT
// retried here — the SDK's built-in retry handles those.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "unexpected EOF") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "TLS handshake timeout")
}

func waitRetry(ctx context.Context, attempt int) bool {
	delay := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, ...
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

func retry0(ctx context.Context, maxRetries int, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil || !IsTransient(lastErr) {
			return lastErr
		}
		if attempt < maxRetries {
			if !waitRetry(ctx, attempt) {
				return ctx.Err()
			}
		}
	}
	return lastErr
}

func retry1[T any](ctx context.Context, maxRetries int, fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, err := fn()
		if err == nil || !IsTransient(err) {
			return result, err
		}
		lastErr = err
		if attempt < maxRetries {
			if !waitRetry(ctx, attempt) {
				return zero, ctx.Err()
			}
		}
	}
	return zero, lastErr
}

// --- Tunnel operations ---

func (r *retryClient) CreateTunnel(ctx context.Context, name string) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.CreateTunnel(ctx, name)
	})
}

func (r *retryClient) GetTunnelIDByName(ctx context.Context, name string) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.GetTunnelIDByName(ctx, name)
	})
}

func (r *retryClient) ListTunnels(ctx context.Context) ([]Tunnel, error) {
	return retry1(ctx, r.maxRetries, func() ([]Tunnel, error) {
		return r.inner.ListTunnels(ctx)
	})
}

func (r *retryClient) CleanupTunnelConnections(ctx context.Context, tunnelID string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.CleanupTunnelConnections(ctx, tunnelID)
	})
}

func (r *retryClient) DeleteTunnel(ctx context.Context, tunnelID string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.DeleteTunnel(ctx, tunnelID)
	})
}

func (r *retryClient) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.GetTunnelToken(ctx, tunnelID)
	})
}

// --- Zone/DNS operations ---

func (r *retryClient) ListZoneIDs(ctx context.Context) ([]string, error) {
	return retry1(ctx, r.maxRetries, func() ([]string, error) {
		return r.inner.ListZoneIDs(ctx)
	})
}

func (r *retryClient) FindZoneIDByHostname(ctx context.Context, hostname string) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.FindZoneIDByHostname(ctx, hostname)
	})
}

func (r *retryClient) EnsureDNSCNAME(ctx context.Context, zoneID, hostname, target string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.EnsureDNSCNAME(ctx, zoneID, hostname, target)
	})
}

func (r *retryClient) DeleteDNSCNAME(ctx context.Context, zoneID, hostname string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.DeleteDNSCNAME(ctx, zoneID, hostname)
	})
}

func (r *retryClient) ListDNSCNAMEsByTarget(ctx context.Context, zoneID, target string) ([]string, error) {
	return retry1(ctx, r.maxRetries, func() ([]string, error) {
		return r.inner.ListDNSCNAMEsByTarget(ctx, zoneID, target)
	})
}
