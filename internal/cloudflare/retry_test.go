// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare_test

import (
	"context"
	"fmt"
	"io"
	"testing"

	cfgo "github.com/cloudflare/cloudflare-go/v6"
	. "github.com/onsi/gomega"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"cloudflare API error", &cfgo.Error{StatusCode: 400}, false},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"wrapped unexpected EOF", fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF), true},
		{"string unexpected EOF", fmt.Errorf("error reading response body: unexpected EOF"), true},
		{"connection reset", fmt.Errorf("read tcp: connection reset by peer"), true},
		{"i/o timeout", fmt.Errorf("dial tcp: i/o timeout"), true},
		{"TLS handshake timeout", fmt.Errorf("TLS handshake timeout"), true},
		{"permanent error", fmt.Errorf("invalid API token"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(cloudflare.IsTransient(tt.err)).To(Equal(tt.want))
		})
	}
}

func TestWithRetry_RetriesTransientErrors(t *testing.T) {
	t.Run("retry1", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)
		calls := 0
		inner := newMockRetryClient()
		inner.listZoneIDsFn = func() ([]string, error) {
			calls++
			if calls <= 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return []string{"zone-1"}, nil
		}
		c := cloudflare.WithRetryMaxRetries(inner, 1)
		ids, err := c.ListZoneIDs(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ids).To(Equal([]string{"zone-1"}))
		g.Expect(calls).To(Equal(2))
	})
	t.Run("retry0", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)
		calls := 0
		inner := newMockRetryClient()
		inner.deleteTunnelFn = func() error {
			calls++
			if calls <= 1 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		c := cloudflare.WithRetryMaxRetries(inner, 1)
		err := c.DeleteTunnel(context.Background(), "tunnel-id")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(calls).To(Equal(2))
	})
	t.Run("retry2", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)
		calls := 0
		inner := newMockRetryClient()
		inner.getPoolByNameFn = func() (string, *cloudflare.PoolConfig, error) {
			calls++
			if calls <= 1 {
				return "", nil, io.ErrUnexpectedEOF
			}
			return "pool-id", &cloudflare.PoolConfig{Name: "pool-1"}, nil
		}
		c := cloudflare.WithRetryMaxRetries(inner, 1)
		id, cfg, err := c.GetPoolByName(context.Background(), "pool-1")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(id).To(Equal("pool-id"))
		g.Expect(cfg.Name).To(Equal("pool-1"))
		g.Expect(calls).To(Equal(2))
	})
}

func TestWithRetry_DoesNotRetryPermanentErrors(t *testing.T) {
	g := NewWithT(t)
	calls := 0
	inner := newMockRetryClient()
	inner.listZoneIDsFn = func() ([]string, error) {
		calls++
		return nil, fmt.Errorf("invalid API token")
	}
	c := cloudflare.WithRetry(inner)
	_, err := c.ListZoneIDs(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("invalid API token"))
	g.Expect(calls).To(Equal(1))
}

func TestWithRetry_RespectsContextCancellation(t *testing.T) {
	t.Run("retry1", func(t *testing.T) {
		g := NewWithT(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		inner := newMockRetryClient()
		inner.listZoneIDsFn = func() ([]string, error) {
			calls++
			return nil, io.ErrUnexpectedEOF
		}
		c := cloudflare.WithRetry(inner)
		_, err := c.ListZoneIDs(ctx)
		g.Expect(err).To(MatchError(context.Canceled))
		g.Expect(calls).To(Equal(1))
	})
	t.Run("retry0", func(t *testing.T) {
		g := NewWithT(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		inner := newMockRetryClient()
		inner.deleteTunnelFn = func() error {
			calls++
			return io.ErrUnexpectedEOF
		}
		c := cloudflare.WithRetry(inner)
		err := c.DeleteTunnel(ctx, "tunnel-id")
		g.Expect(err).To(MatchError(context.Canceled))
		g.Expect(calls).To(Equal(1))
	})
	t.Run("retry2", func(t *testing.T) {
		g := NewWithT(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		inner := newMockRetryClient()
		inner.getPoolByNameFn = func() (string, *cloudflare.PoolConfig, error) {
			calls++
			return "", nil, io.ErrUnexpectedEOF
		}
		c := cloudflare.WithRetry(inner)
		id, cfg, err := c.GetPoolByName(ctx, "pool-1")
		g.Expect(err).To(MatchError(context.Canceled))
		g.Expect(id).To(BeEmpty())
		g.Expect(cfg).To(BeNil())
		g.Expect(calls).To(Equal(1))
	})
}

func TestWithRetry_ExhaustsRetries(t *testing.T) {
	t.Run("retry1", func(t *testing.T) {
		g := NewWithT(t)
		inner := newMockRetryClient()
		inner.listZoneIDsFn = func() ([]string, error) {
			return nil, io.ErrUnexpectedEOF
		}
		c := cloudflare.WithRetryMaxRetries(inner, 0)
		ids, err := c.ListZoneIDs(context.Background())
		g.Expect(err).To(MatchError(io.ErrUnexpectedEOF))
		g.Expect(ids).To(BeNil())
	})
	t.Run("retry0", func(t *testing.T) {
		g := NewWithT(t)
		inner := newMockRetryClient()
		inner.deleteTunnelFn = func() error {
			return io.ErrUnexpectedEOF
		}
		c := cloudflare.WithRetryMaxRetries(inner, 0)
		err := c.DeleteTunnel(context.Background(), "tunnel-id")
		g.Expect(err).To(MatchError(io.ErrUnexpectedEOF))
	})
	t.Run("retry2", func(t *testing.T) {
		g := NewWithT(t)
		inner := newMockRetryClient()
		inner.getPoolByNameFn = func() (string, *cloudflare.PoolConfig, error) {
			return "", nil, io.ErrUnexpectedEOF
		}
		c := cloudflare.WithRetryMaxRetries(inner, 0)
		id, cfg, err := c.GetPoolByName(context.Background(), "pool-1")
		g.Expect(err).To(MatchError(io.ErrUnexpectedEOF))
		g.Expect(id).To(BeEmpty())
		g.Expect(cfg).To(BeNil())
	})
}

func TestWithRetry_DelegatesAllMethods(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	inner := newMockRetryClient()
	c := cloudflare.WithRetry(inner)

	// Tunnel operations.
	tunnelID, err := c.CreateTunnel(ctx, "my-tunnel")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tunnelID).To(Equal("tunnel-id"))
	g.Expect(inner.calls["CreateTunnel"]).To(Equal(1))

	tunnelID, err = c.GetTunnelIDByName(ctx, "my-tunnel")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tunnelID).To(Equal("tunnel-id"))
	g.Expect(inner.calls["GetTunnelIDByName"]).To(Equal(1))

	tunnels, err := c.ListTunnels(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tunnels).To(Equal([]cloudflare.Tunnel{{ID: "t1", Name: "tunnel-1"}}))
	g.Expect(inner.calls["ListTunnels"]).To(Equal(1))

	err = c.CleanupTunnelConnections(ctx, "tunnel-id")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["CleanupTunnelConnections"]).To(Equal(1))

	err = c.DeleteTunnel(ctx, "tunnel-id")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["DeleteTunnel"]).To(Equal(1))

	token, err := c.GetTunnelToken(ctx, "tunnel-id")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(token).To(Equal("token-value"))
	g.Expect(inner.calls["GetTunnelToken"]).To(Equal(1))

	ingress, err := c.GetTunnelConfiguration(ctx, "tunnel-id")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ingress).To(Equal([]cloudflare.IngressRule{{Hostname: "app.example.com", Service: "http://localhost:8080"}}))
	g.Expect(inner.calls["GetTunnelConfiguration"]).To(Equal(1))

	err = c.UpdateTunnelConfiguration(ctx, "tunnel-id", []cloudflare.IngressRule{{Hostname: "app.example.com", Service: "http://localhost:8080"}})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["UpdateTunnelConfiguration"]).To(Equal(1))

	// Zone/DNS operations.
	zoneIDs, err := c.ListZoneIDs(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(zoneIDs).To(Equal([]string{"zone-1"}))
	g.Expect(inner.calls["ListZoneIDs"]).To(Equal(1))

	zoneID, err := c.FindZoneIDByHostname(ctx, "example.com")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(zoneID).To(Equal("zone-1"))
	g.Expect(inner.calls["FindZoneIDByHostname"]).To(Equal(1))

	err = c.EnsureDNSCNAME(ctx, "zone-1", "app.example.com", "target.example.com")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["EnsureDNSCNAME"]).To(Equal(1))

	err = c.DeleteDNSCNAME(ctx, "zone-1", "app.example.com")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["DeleteDNSCNAME"]).To(Equal(1))

	hostnames, err := c.ListDNSCNAMEsByTarget(ctx, "zone-1", "target.example.com")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hostnames).To(Equal([]string{"app.example.com"}))
	g.Expect(inner.calls["ListDNSCNAMEsByTarget"]).To(Equal(1))

	// Monitor operations.
	monitorID, err := c.CreateMonitor(ctx, "my-monitor", cloudflare.MonitorConfig{Type: "https"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(monitorID).To(Equal("monitor-id"))
	g.Expect(inner.calls["CreateMonitor"]).To(Equal(1))

	monitorID, err = c.GetMonitorByName(ctx, "my-monitor")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(monitorID).To(Equal("monitor-id"))
	g.Expect(inner.calls["GetMonitorByName"]).To(Equal(1))

	err = c.UpdateMonitor(ctx, "monitor-id", "my-monitor", cloudflare.MonitorConfig{Type: "https"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["UpdateMonitor"]).To(Equal(1))

	err = c.DeleteMonitor(ctx, "monitor-id")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["DeleteMonitor"]).To(Equal(1))

	// Pool operations.
	poolID, err := c.CreatePool(ctx, cloudflare.PoolConfig{Name: "pool-1"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(poolID).To(Equal("pool-id"))
	g.Expect(inner.calls["CreatePool"]).To(Equal(1))

	poolID, poolCfg, err := c.GetPoolByName(ctx, "pool-1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(poolID).To(Equal("pool-id"))
	g.Expect(poolCfg.Name).To(Equal("pool-1"))
	g.Expect(inner.calls["GetPoolByName"]).To(Equal(1))

	err = c.UpdatePool(ctx, "pool-id", cloudflare.PoolConfig{Name: "pool-1"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["UpdatePool"]).To(Equal(1))

	err = c.DeletePool(ctx, "pool-id")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["DeletePool"]).To(Equal(1))

	pools, err := c.ListPoolsByPrefix(ctx, "pool-")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pools).To(Equal([]cloudflare.LoadBalancerPool{{ID: "p1", Name: "pool-1"}}))
	g.Expect(inner.calls["ListPoolsByPrefix"]).To(Equal(1))

	// Load Balancer operations.
	err = c.EnsureLoadBalancer(ctx, "zone-1", "app.example.com", []string{"pool-1"}, "random", "none", map[string]float64{"pool-1": 1.0})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["EnsureLoadBalancer"]).To(Equal(1))

	err = c.DeleteLoadBalancer(ctx, "zone-1", "app.example.com")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inner.calls["DeleteLoadBalancer"]).To(Equal(1))

	lbHostnames, err := c.ListLoadBalancerHostnames(ctx, "zone-1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(lbHostnames).To(Equal([]string{"app.example.com"}))
	g.Expect(inner.calls["ListLoadBalancerHostnames"]).To(Equal(1))
}

// mockRetryClient implements cloudflare.Client with configurable overrides
// for retry testing and call tracking for delegation verification.
type mockRetryClient struct {
	calls map[string]int

	// Overrides for retry behavior tests (nil = use default).
	deleteTunnelFn  func() error
	listZoneIDsFn   func() ([]string, error)
	getPoolByNameFn func() (string, *cloudflare.PoolConfig, error)
}

func newMockRetryClient() *mockRetryClient {
	return &mockRetryClient{calls: make(map[string]int)}
}

func (m *mockRetryClient) CreateTunnel(_ context.Context, _ string) (string, error) {
	m.calls["CreateTunnel"]++
	return "tunnel-id", nil
}

func (m *mockRetryClient) GetTunnelIDByName(_ context.Context, _ string) (string, error) {
	m.calls["GetTunnelIDByName"]++
	return "tunnel-id", nil
}

func (m *mockRetryClient) ListTunnels(_ context.Context) ([]cloudflare.Tunnel, error) {
	m.calls["ListTunnels"]++
	return []cloudflare.Tunnel{{ID: "t1", Name: "tunnel-1"}}, nil
}

func (m *mockRetryClient) CleanupTunnelConnections(_ context.Context, _ string) error {
	m.calls["CleanupTunnelConnections"]++
	return nil
}

func (m *mockRetryClient) DeleteTunnel(_ context.Context, _ string) error {
	m.calls["DeleteTunnel"]++
	if m.deleteTunnelFn != nil {
		return m.deleteTunnelFn()
	}
	return nil
}

func (m *mockRetryClient) GetTunnelToken(_ context.Context, _ string) (string, error) {
	m.calls["GetTunnelToken"]++
	return "token-value", nil
}

func (m *mockRetryClient) GetTunnelConfiguration(_ context.Context, _ string) ([]cloudflare.IngressRule, error) {
	m.calls["GetTunnelConfiguration"]++
	return []cloudflare.IngressRule{{Hostname: "app.example.com", Service: "http://localhost:8080"}}, nil
}

func (m *mockRetryClient) UpdateTunnelConfiguration(_ context.Context, _ string, _ []cloudflare.IngressRule) error {
	m.calls["UpdateTunnelConfiguration"]++
	return nil
}

func (m *mockRetryClient) ListZoneIDs(_ context.Context) ([]string, error) {
	m.calls["ListZoneIDs"]++
	if m.listZoneIDsFn != nil {
		return m.listZoneIDsFn()
	}
	return []string{"zone-1"}, nil
}

func (m *mockRetryClient) FindZoneIDByHostname(_ context.Context, _ string) (string, error) {
	m.calls["FindZoneIDByHostname"]++
	return "zone-1", nil
}

func (m *mockRetryClient) EnsureDNSCNAME(_ context.Context, _, _, _ string) error {
	m.calls["EnsureDNSCNAME"]++
	return nil
}

func (m *mockRetryClient) DeleteDNSCNAME(_ context.Context, _, _ string) error {
	m.calls["DeleteDNSCNAME"]++
	return nil
}

func (m *mockRetryClient) ListDNSCNAMEsByTarget(_ context.Context, _, _ string) ([]string, error) {
	m.calls["ListDNSCNAMEsByTarget"]++
	return []string{"app.example.com"}, nil
}

func (m *mockRetryClient) CreateMonitor(_ context.Context, _ string, _ cloudflare.MonitorConfig) (string, error) {
	m.calls["CreateMonitor"]++
	return "monitor-id", nil
}

func (m *mockRetryClient) GetMonitorByName(_ context.Context, _ string) (string, error) {
	m.calls["GetMonitorByName"]++
	return "monitor-id", nil
}

func (m *mockRetryClient) UpdateMonitor(_ context.Context, _, _ string, _ cloudflare.MonitorConfig) error {
	m.calls["UpdateMonitor"]++
	return nil
}

func (m *mockRetryClient) DeleteMonitor(_ context.Context, _ string) error {
	m.calls["DeleteMonitor"]++
	return nil
}

func (m *mockRetryClient) CreatePool(_ context.Context, _ cloudflare.PoolConfig) (string, error) {
	m.calls["CreatePool"]++
	return "pool-id", nil
}

func (m *mockRetryClient) GetPoolByName(_ context.Context, _ string) (string, *cloudflare.PoolConfig, error) {
	m.calls["GetPoolByName"]++
	if m.getPoolByNameFn != nil {
		return m.getPoolByNameFn()
	}
	return "pool-id", &cloudflare.PoolConfig{Name: "pool-1"}, nil
}

func (m *mockRetryClient) UpdatePool(_ context.Context, _ string, _ cloudflare.PoolConfig) error {
	m.calls["UpdatePool"]++
	return nil
}

func (m *mockRetryClient) DeletePool(_ context.Context, _ string) error {
	m.calls["DeletePool"]++
	return nil
}

func (m *mockRetryClient) ListPoolsByPrefix(_ context.Context, _ string) ([]cloudflare.LoadBalancerPool, error) {
	m.calls["ListPoolsByPrefix"]++
	return []cloudflare.LoadBalancerPool{{ID: "p1", Name: "pool-1"}}, nil
}

func (m *mockRetryClient) EnsureLoadBalancer(_ context.Context, _, _ string, _ []string, _, _ string, _ map[string]float64) error {
	m.calls["EnsureLoadBalancer"]++
	return nil
}

func (m *mockRetryClient) DeleteLoadBalancer(_ context.Context, _, _ string) error {
	m.calls["DeleteLoadBalancer"]++
	return nil
}

func (m *mockRetryClient) ListLoadBalancerHostnames(_ context.Context, _ string) ([]string, error) {
	m.calls["ListLoadBalancerHostnames"]++
	return []string{"app.example.com"}, nil
}
