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

func retry2[T1, T2 any](ctx context.Context, maxRetries int, fn func() (T1, T2, error)) (T1, T2, error) {
	var zero1 T1
	var zero2 T2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		r1, r2, err := fn()
		if err == nil || !IsTransient(err) {
			return r1, r2, err
		}
		lastErr = err
		if attempt < maxRetries {
			if !waitRetry(ctx, attempt) {
				return zero1, zero2, ctx.Err()
			}
		}
	}
	return zero1, zero2, lastErr
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

func (r *retryClient) GetTunnelConfiguration(ctx context.Context, tunnelID string) ([]IngressRule, error) {
	return retry1(ctx, r.maxRetries, func() ([]IngressRule, error) {
		return r.inner.GetTunnelConfiguration(ctx, tunnelID)
	})
}

func (r *retryClient) UpdateTunnelConfiguration(ctx context.Context, tunnelID string, ingress []IngressRule) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.UpdateTunnelConfiguration(ctx, tunnelID, ingress)
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

// --- Load Balancer Monitor operations ---

func (r *retryClient) CreateMonitor(ctx context.Context, name string, config MonitorConfig) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.CreateMonitor(ctx, name, config)
	})
}

func (r *retryClient) GetMonitorByName(ctx context.Context, name string) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.GetMonitorByName(ctx, name)
	})
}

func (r *retryClient) UpdateMonitor(ctx context.Context, monitorID, name string, config MonitorConfig) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.UpdateMonitor(ctx, monitorID, name, config)
	})
}

func (r *retryClient) DeleteMonitor(ctx context.Context, monitorID string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.DeleteMonitor(ctx, monitorID)
	})
}

// --- Load Balancer Pool operations ---

func (r *retryClient) CreatePool(ctx context.Context, config PoolConfig) (string, error) {
	return retry1(ctx, r.maxRetries, func() (string, error) {
		return r.inner.CreatePool(ctx, config)
	})
}

func (r *retryClient) GetPoolByName(ctx context.Context, name string) (string, *PoolConfig, error) {
	return retry2(ctx, r.maxRetries, func() (string, *PoolConfig, error) {
		return r.inner.GetPoolByName(ctx, name)
	})
}

func (r *retryClient) UpdatePool(ctx context.Context, poolID string, config PoolConfig) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.UpdatePool(ctx, poolID, config)
	})
}

func (r *retryClient) DeletePool(ctx context.Context, poolID string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.DeletePool(ctx, poolID)
	})
}

func (r *retryClient) ListPoolsByPrefix(ctx context.Context, prefix string) ([]LoadBalancerPool, error) {
	return retry1(ctx, r.maxRetries, func() ([]LoadBalancerPool, error) {
		return r.inner.ListPoolsByPrefix(ctx, prefix)
	})
}

// --- Load Balancer operations ---

func (r *retryClient) EnsureLoadBalancer(ctx context.Context, zoneID, hostname string, poolIDs []string, steeringPolicy, sessionAffinity string, poolWeights map[string]float64) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.EnsureLoadBalancer(ctx, zoneID, hostname, poolIDs, steeringPolicy, sessionAffinity, poolWeights)
	})
}

func (r *retryClient) DeleteLoadBalancer(ctx context.Context, zoneID, hostname string) error {
	return retry0(ctx, r.maxRetries, func() error {
		return r.inner.DeleteLoadBalancer(ctx, zoneID, hostname)
	})
}

func (r *retryClient) ListLoadBalancerHostnames(ctx context.Context, zoneID string) ([]string, error) {
	return retry1(ctx, r.maxRetries, func() ([]string, error) {
		return r.inner.ListLoadBalancerHostnames(ctx, zoneID)
	})
}
