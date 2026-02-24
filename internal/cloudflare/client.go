// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/load_balancers"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/cloudflare/cloudflare-go/v6/zones"
)

// ClientConfig holds the credentials needed to interact with the Cloudflare API.
type ClientConfig struct {
	APIToken  string
	AccountID string
	BaseURL   string // optional override for testing
}

// IngressRule represents a Cloudflare tunnel ingress rule mapping a hostname to a service.
type IngressRule struct {
	Hostname string
	Service  string
	Path     string
}

// Tunnel represents a Cloudflare tunnel with its ID and name.
type Tunnel struct {
	ID   string
	Name string
}

// MonitorConfig holds the configuration for a Cloudflare LB health monitor.
type MonitorConfig struct {
	Type          string // "http", "https", or "tcp". Default: "https".
	Path          string // Health check endpoint. Default: "/ready".
	Interval      int64  // Seconds between checks. Default: 60.
	Timeout       int64  // Seconds before failure. Default: 5.
	ExpectedCodes string // Expected HTTP response codes. Default: "200".
}

// PoolOrigin represents an origin server within a Cloudflare LB pool.
type PoolOrigin struct {
	Name    string  // Origin name (e.g. AZ name or service-az key).
	Address string  // Origin address (e.g. <tunnelID>.cfargotunnel.com).
	Enabled bool    // Whether this origin is enabled.
	Weight  float64 // Origin-level weight (0-1). Default: 1.
}

// PoolConfig holds the configuration for a Cloudflare LB pool.
type PoolConfig struct {
	Name        string       // Pool name.
	Description string       // Pool description (ownership metadata).
	MonitorID   string       // Associated monitor ID.
	Origins     []PoolOrigin // Pool origins.
	Weight      float64      // Pool-level weight for LB steering (0-1). Default: 1.
	Enabled     bool         // Whether this pool is enabled.
}

// LoadBalancerPool represents a Cloudflare LB pool with its ID and name.
type LoadBalancerPool struct {
	ID   string
	Name string
}

// Client abstracts Cloudflare tunnel operations.
type Client interface {
	// Tunnel operations.
	CreateTunnel(ctx context.Context, name string) (tunnelID string, err error)
	GetTunnelIDByName(ctx context.Context, name string) (tunnelID string, err error)
	ListTunnels(ctx context.Context) ([]Tunnel, error)
	CleanupTunnelConnections(ctx context.Context, tunnelID string) error
	DeleteTunnel(ctx context.Context, tunnelID string) error
	GetTunnelToken(ctx context.Context, tunnelID string) (token string, err error)
	GetTunnelConfiguration(ctx context.Context, tunnelID string) ([]IngressRule, error)
	UpdateTunnelConfiguration(ctx context.Context, tunnelID string, ingress []IngressRule) error

	// Zone/DNS operations.
	ListZoneIDs(ctx context.Context) ([]string, error)
	FindZoneIDByHostname(ctx context.Context, hostname string) (string, error)
	EnsureDNSCNAME(ctx context.Context, zoneID, hostname, target, comment string) error
	DeleteDNSCNAME(ctx context.Context, zoneID, hostname string) error
	ListDNSCNAMEsByTarget(ctx context.Context, zoneID, target string) ([]string, error)

	// Load Balancer Monitor operations (account-level).
	CreateMonitor(ctx context.Context, name, description string, config MonitorConfig) (monitorID string, err error)
	GetMonitorByName(ctx context.Context, name string) (monitorID string, err error)
	UpdateMonitor(ctx context.Context, monitorID, name, description string, config MonitorConfig) error
	DeleteMonitor(ctx context.Context, monitorID string) error

	// Load Balancer Pool operations (account-level).
	CreatePool(ctx context.Context, config PoolConfig) (poolID string, err error)
	GetPoolByName(ctx context.Context, name string) (poolID string, pool *PoolConfig, err error)
	UpdatePool(ctx context.Context, poolID string, config PoolConfig) error
	DeletePool(ctx context.Context, poolID string) error
	ListPoolsByPrefix(ctx context.Context, prefix string) ([]LoadBalancerPool, error)

	// Load Balancer operations (zone-level).
	EnsureLoadBalancer(ctx context.Context, zoneID, hostname string, poolIDs []string, steeringPolicy, sessionAffinity, description string, poolWeights map[string]float64) error
	DeleteLoadBalancer(ctx context.Context, zoneID, hostname string) error
	ListLoadBalancerHostnames(ctx context.Context, zoneID string) ([]string, error)
}

// IsConflict reports whether the error is a 409 Conflict from the Cloudflare API.
func IsConflict(err error) bool {
	var apiErr *cloudflare.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// IsNotFound reports whether the error is a 404 Not Found from the Cloudflare API.
func IsNotFound(err error) bool {
	var apiErr *cloudflare.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// TunnelTarget returns the CNAME target for a Cloudflare tunnel.
func TunnelTarget(tunnelID string) string {
	return tunnelID + ".cfargotunnel.com"
}

// ClientFactory creates a Client from a ClientConfig.
type ClientFactory func(cfg ClientConfig) (Client, error)

// NewClient creates a new Client backed by the Cloudflare API.
func NewClient(cfg ClientConfig) (Client, error) {
	opts := []option.RequestOption{option.WithAPIToken(cfg.APIToken)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &client{
		client:    cloudflare.NewClient(opts...),
		accountID: cfg.AccountID,
	}, nil
}

type client struct {
	client    *cloudflare.Client
	accountID string
}

// --- Tunnel operations ---

func (c *client) CreateTunnel(ctx context.Context, name string) (string, error) {
	tunnel, err := c.client.ZeroTrust.Tunnels.Cloudflared.New(ctx, zero_trust.TunnelCloudflaredNewParams{
		AccountID: cloudflare.String(c.accountID),
		Name:      cloudflare.String(name),
		ConfigSrc: cloudflare.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcCloudflare),
	})
	if err != nil {
		return "", err
	}
	return tunnel.ID, nil
}

func (c *client) GetTunnelIDByName(ctx context.Context, name string) (string, error) {
	pager := c.client.ZeroTrust.Tunnels.Cloudflared.ListAutoPaging(ctx, zero_trust.TunnelCloudflaredListParams{
		AccountID: cloudflare.String(c.accountID),
		Name:      cloudflare.String(name),
		IsDeleted: cloudflare.Bool(false),
	})
	for pager.Next() {
		tunnel := pager.Current()
		if tunnel.Name == name {
			return tunnel.ID, nil
		}
	}
	if err := pager.Err(); err != nil {
		return "", fmt.Errorf("listing tunnels by name %q: %w", name, err)
	}
	return "", nil
}

func (c *client) ListTunnels(ctx context.Context) ([]Tunnel, error) {
	pager := c.client.ZeroTrust.Tunnels.Cloudflared.ListAutoPaging(ctx, zero_trust.TunnelCloudflaredListParams{
		AccountID: cloudflare.String(c.accountID),
		IsDeleted: cloudflare.Bool(false),
	})
	var tunnels []Tunnel
	for pager.Next() {
		t := pager.Current()
		tunnels = append(tunnels, Tunnel{ID: t.ID, Name: t.Name})
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("listing tunnels: %w", err)
	}
	return tunnels, nil
}

func (c *client) CleanupTunnelConnections(ctx context.Context, tunnelID string) error {
	_, err := c.client.ZeroTrust.Tunnels.Cloudflared.Connections.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredConnectionDeleteParams{
		AccountID: cloudflare.String(c.accountID),
	})
	return err
}

func (c *client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	_, err := c.client.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredDeleteParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (c *client) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	token, err := c.client.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if err != nil {
		return "", err
	}
	return *token, nil
}

func (c *client) GetTunnelConfiguration(ctx context.Context, tunnelID string) ([]IngressRule, error) {
	resp, err := c.client.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredConfigurationGetParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if err != nil {
		return nil, err
	}
	rules := make([]IngressRule, 0, len(resp.Config.Ingress))
	for _, r := range resp.Config.Ingress {
		rules = append(rules, IngressRule{
			Hostname: r.Hostname,
			Service:  r.Service,
			Path:     r.Path,
		})
	}
	return rules, nil
}

func (c *client) UpdateTunnelConfiguration(ctx context.Context, tunnelID string, ingress []IngressRule) error {
	sdkIngress := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(ingress))
	for _, r := range ingress {
		entry := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(r.Hostname),
			Service:  cloudflare.F(r.Service),
		}
		if r.Path != "" {
			entry.Path = cloudflare.F(r.Path)
		}
		sdkIngress = append(sdkIngress, entry)
	}
	_, err := c.client.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, tunnelID, zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.String(c.accountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(sdkIngress),
		}),
	})
	return err
}

// --- Zone/DNS operations ---

func (c *client) ListZoneIDs(ctx context.Context) ([]string, error) {
	pager := c.client.Zones.ListAutoPaging(ctx, zones.ZoneListParams{})
	var ids []string
	for pager.Next() {
		ids = append(ids, pager.Current().ID)
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}
	return ids, nil
}

func (c *client) FindZoneIDByHostname(ctx context.Context, hostname string) (string, error) {
	// Strip subdomain levels progressively until we find a matching zone.
	parts := strings.Split(hostname, ".")
	for i := range len(parts) - 1 {
		candidate := strings.Join(parts[i:], ".")
		pager := c.client.Zones.ListAutoPaging(ctx, zones.ZoneListParams{
			Name: cloudflare.F(candidate),
		})
		for pager.Next() {
			zone := pager.Current()
			if zone.Name == candidate {
				return zone.ID, nil
			}
		}
		if err := pager.Err(); err != nil {
			return "", fmt.Errorf("listing zones for %q: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("no zone found for hostname %q", hostname)
}

func (c *client) EnsureDNSCNAME(ctx context.Context, zoneID, hostname, target, comment string) error {
	pager := c.client.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(hostname)}),
		Type:   cloudflare.F(dns.RecordListParamsTypeCNAME),
	})
	for pager.Next() {
		record := pager.Current()
		if record.Name == hostname {
			if record.Content == target {
				return nil
			}
			_, err := c.client.DNS.Records.Update(ctx, record.ID, dns.RecordUpdateParams{
				ZoneID: cloudflare.F(zoneID),
				Body: dns.CNAMERecordParam{
					Name:    cloudflare.F(hostname),
					Content: cloudflare.F(target),
					Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
					TTL:     cloudflare.F(dns.TTL1),
					Proxied: cloudflare.F(true),
					Comment: cloudflare.String(comment),
				},
			})
			return err
		}
	}
	if err := pager.Err(); err != nil {
		return fmt.Errorf("listing DNS records for %q: %w", hostname, err)
	}
	_, err := c.client.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(zoneID),
		Body: dns.CNAMERecordParam{
			Name:    cloudflare.F(hostname),
			Content: cloudflare.F(target),
			Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
			TTL:     cloudflare.F(dns.TTL1),
			Proxied: cloudflare.F(true),
			Comment: cloudflare.String(comment),
		},
	})
	return err
}

func (c *client) DeleteDNSCNAME(ctx context.Context, zoneID, hostname string) error {
	pager := c.client.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(hostname)}),
		Type:   cloudflare.F(dns.RecordListParamsTypeCNAME),
	})
	for pager.Next() {
		record := pager.Current()
		if record.Name == hostname {
			_, err := c.client.DNS.Records.Delete(ctx, record.ID, dns.RecordDeleteParams{
				ZoneID: cloudflare.F(zoneID),
			})
			return err
		}
	}
	return pager.Err()
}

func (c *client) ListDNSCNAMEsByTarget(ctx context.Context, zoneID, target string) ([]string, error) {
	pager := c.client.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID:  cloudflare.F(zoneID),
		Type:    cloudflare.F(dns.RecordListParamsTypeCNAME),
		Content: cloudflare.F(dns.RecordListParamsContent{Exact: cloudflare.F(target)}),
	})
	var hostnames []string
	for pager.Next() {
		record := pager.Current()
		if record.Content == target {
			hostnames = append(hostnames, record.Name)
		}
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("listing CNAME records by target %q in zone %q: %w", target, zoneID, err)
	}
	return hostnames, nil
}

// --- Load Balancer Monitor operations ---

func (c *client) CreateMonitor(ctx context.Context, name, description string, config MonitorConfig) (string, error) {
	desc := monitorDescription(name, description)
	params := load_balancers.MonitorNewParams{
		AccountID:     cloudflare.String(c.accountID),
		Description:   cloudflare.String(desc),
		Type:          cloudflare.F(load_balancers.MonitorNewParamsType(config.Type)),
		Path:          cloudflare.String(config.Path),
		ExpectedCodes: cloudflare.String(config.ExpectedCodes),
	}
	if config.Interval > 0 {
		params.Interval = cloudflare.Int(config.Interval)
	}
	if config.Timeout > 0 {
		params.Timeout = cloudflare.Int(config.Timeout)
	}
	monitor, err := c.client.LoadBalancers.Monitors.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("creating monitor %q: %w", name, err)
	}
	return monitor.ID, nil
}

func (c *client) GetMonitorByName(ctx context.Context, name string) (string, error) {
	prefix := name + " | "
	page, err := c.client.LoadBalancers.Monitors.List(ctx, load_balancers.MonitorListParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if err != nil {
		return "", fmt.Errorf("listing monitors: %w", err)
	}
	for _, m := range page.Result {
		if strings.HasPrefix(m.Description, prefix) || m.Description == name {
			return m.ID, nil
		}
	}
	return "", nil
}

func (c *client) UpdateMonitor(ctx context.Context, monitorID, name, description string, config MonitorConfig) error {
	desc := monitorDescription(name, description)
	params := load_balancers.MonitorUpdateParams{
		AccountID:     cloudflare.String(c.accountID),
		Description:   cloudflare.String(desc),
		Type:          cloudflare.F(load_balancers.MonitorUpdateParamsType(config.Type)),
		Path:          cloudflare.String(config.Path),
		ExpectedCodes: cloudflare.String(config.ExpectedCodes),
	}
	if config.Interval > 0 {
		params.Interval = cloudflare.Int(config.Interval)
	}
	if config.Timeout > 0 {
		params.Timeout = cloudflare.Int(config.Timeout)
	}
	_, err := c.client.LoadBalancers.Monitors.Update(ctx, monitorID, params)
	if err != nil {
		return fmt.Errorf("updating monitor %q: %w", monitorID, err)
	}
	return nil
}

// monitorDescription builds the Description field for a monitor, encoding both
// the hash name (used for lookup) and the ownership metadata.
func monitorDescription(name, description string) string {
	return name + " | " + description
}

func (c *client) DeleteMonitor(ctx context.Context, monitorID string) error {
	_, err := c.client.LoadBalancers.Monitors.Delete(ctx, monitorID, load_balancers.MonitorDeleteParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting monitor %q: %w", monitorID, err)
	}
	return nil
}

// --- Load Balancer Pool operations ---

func (c *client) CreatePool(ctx context.Context, config PoolConfig) (string, error) {
	origins := make([]load_balancers.OriginParam, 0, len(config.Origins))
	for _, o := range config.Origins {
		origins = append(origins, load_balancers.OriginParam{
			Name:    cloudflare.String(o.Name),
			Address: cloudflare.String(o.Address),
			Enabled: cloudflare.Bool(o.Enabled),
			Weight:  cloudflare.Float(o.Weight),
		})
	}
	pool, err := c.client.LoadBalancers.Pools.New(ctx, load_balancers.PoolNewParams{
		AccountID:   cloudflare.String(c.accountID),
		Name:        cloudflare.String(config.Name),
		Description: cloudflare.String(config.Description),
		Origins:     cloudflare.F(origins),
		Monitor:     cloudflare.String(config.MonitorID),
		Enabled:     cloudflare.Bool(config.Enabled),
	})
	if err != nil {
		return "", fmt.Errorf("creating pool %q: %w", config.Name, err)
	}
	return pool.ID, nil
}

func (c *client) GetPoolByName(ctx context.Context, name string) (string, *PoolConfig, error) {
	page, err := c.client.LoadBalancers.Pools.List(ctx, load_balancers.PoolListParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if err != nil {
		return "", nil, fmt.Errorf("listing pools: %w", err)
	}
	for _, p := range page.Result {
		if p.Name == name {
			origins := make([]PoolOrigin, 0, len(p.Origins))
			for _, o := range p.Origins {
				origins = append(origins, PoolOrigin{
					Name:    o.Name,
					Address: o.Address,
					Enabled: o.Enabled,
					Weight:  o.Weight,
				})
			}
			return p.ID, &PoolConfig{
				Name:      p.Name,
				MonitorID: p.Monitor,
				Origins:   origins,
				Enabled:   p.Enabled,
			}, nil
		}
	}
	return "", nil, nil
}

func (c *client) UpdatePool(ctx context.Context, poolID string, config PoolConfig) error {
	origins := make([]load_balancers.OriginParam, 0, len(config.Origins))
	for _, o := range config.Origins {
		origins = append(origins, load_balancers.OriginParam{
			Name:    cloudflare.String(o.Name),
			Address: cloudflare.String(o.Address),
			Enabled: cloudflare.Bool(o.Enabled),
			Weight:  cloudflare.Float(o.Weight),
		})
	}
	_, err := c.client.LoadBalancers.Pools.Update(ctx, poolID, load_balancers.PoolUpdateParams{
		AccountID:   cloudflare.String(c.accountID),
		Name:        cloudflare.String(config.Name),
		Description: cloudflare.String(config.Description),
		Origins:     cloudflare.F(origins),
		Monitor:     cloudflare.String(config.MonitorID),
		Enabled:     cloudflare.Bool(config.Enabled),
	})
	if err != nil {
		return fmt.Errorf("updating pool %q: %w", poolID, err)
	}
	return nil
}

func (c *client) DeletePool(ctx context.Context, poolID string) error {
	_, err := c.client.LoadBalancers.Pools.Delete(ctx, poolID, load_balancers.PoolDeleteParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting pool %q: %w", poolID, err)
	}
	return nil
}

func (c *client) ListPoolsByPrefix(ctx context.Context, prefix string) ([]LoadBalancerPool, error) {
	page, err := c.client.LoadBalancers.Pools.List(ctx, load_balancers.PoolListParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pools: %w", err)
	}
	var pools []LoadBalancerPool
	for _, p := range page.Result {
		if strings.HasPrefix(p.Name, prefix) {
			pools = append(pools, LoadBalancerPool{ID: p.ID, Name: p.Name})
		}
	}
	return pools, nil
}

// --- Load Balancer operations ---

func (c *client) EnsureLoadBalancer(ctx context.Context, zoneID, hostname string, poolIDs []string, steeringPolicy, sessionAffinity, description string, poolWeights map[string]float64) error {
	// List existing LBs to find one matching the hostname.
	page, err := c.client.LoadBalancers.List(ctx, load_balancers.LoadBalancerListParams{
		ZoneID: cloudflare.String(zoneID),
	})
	if err != nil {
		return fmt.Errorf("listing load balancers: %w", err)
	}

	// Build params shared between create and update.
	defaultPools := make([]string, len(poolIDs))
	copy(defaultPools, poolIDs)

	var randomSteering *load_balancers.RandomSteeringParam
	if len(poolWeights) > 0 {
		randomSteering = &load_balancers.RandomSteeringParam{
			DefaultWeight: cloudflare.Float(1),
			PoolWeights:   cloudflare.F(poolWeights),
		}
	}

	for _, lb := range page.Result {
		if lb.Name != hostname {
			continue
		}
		// Found existing LB — update it.
		params := load_balancers.LoadBalancerUpdateParams{
			ZoneID:          cloudflare.String(zoneID),
			Name:            cloudflare.String(hostname),
			Description:     cloudflare.String(description),
			DefaultPools:    cloudflare.F(defaultPools),
			FallbackPool:    cloudflare.String(poolIDs[0]),
			Proxied:         cloudflare.Bool(true),
			SteeringPolicy:  cloudflare.F(load_balancers.SteeringPolicy(steeringPolicy)),
			SessionAffinity: cloudflare.F(load_balancers.SessionAffinity(sessionAffinity)),
		}
		if randomSteering != nil {
			params.RandomSteering = cloudflare.F(*randomSteering)
		}
		_, err := c.client.LoadBalancers.Update(ctx, lb.ID, params)
		if err != nil {
			return fmt.Errorf("updating load balancer %q: %w", hostname, err)
		}
		return nil
	}

	// No existing LB — create one.
	params := load_balancers.LoadBalancerNewParams{
		ZoneID:          cloudflare.String(zoneID),
		Name:            cloudflare.String(hostname),
		Description:     cloudflare.String(description),
		DefaultPools:    cloudflare.F(defaultPools),
		FallbackPool:    cloudflare.String(poolIDs[0]),
		Proxied:         cloudflare.Bool(true),
		SteeringPolicy:  cloudflare.F(load_balancers.SteeringPolicy(steeringPolicy)),
		SessionAffinity: cloudflare.F(load_balancers.SessionAffinity(sessionAffinity)),
	}
	if randomSteering != nil {
		params.RandomSteering = cloudflare.F(*randomSteering)
	}
	_, err = c.client.LoadBalancers.New(ctx, params)
	if err != nil {
		return fmt.Errorf("creating load balancer %q: %w", hostname, err)
	}
	return nil
}

func (c *client) DeleteLoadBalancer(ctx context.Context, zoneID, hostname string) error {
	page, err := c.client.LoadBalancers.List(ctx, load_balancers.LoadBalancerListParams{
		ZoneID: cloudflare.String(zoneID),
	})
	if err != nil {
		return fmt.Errorf("listing load balancers: %w", err)
	}
	for _, lb := range page.Result {
		if lb.Name == hostname {
			_, err := c.client.LoadBalancers.Delete(ctx, lb.ID, load_balancers.LoadBalancerDeleteParams{
				ZoneID: cloudflare.String(zoneID),
			})
			if IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("deleting load balancer %q: %w", hostname, err)
			}
			return nil
		}
	}
	return nil
}

func (c *client) ListLoadBalancerHostnames(ctx context.Context, zoneID string) ([]string, error) {
	page, err := c.client.LoadBalancers.List(ctx, load_balancers.LoadBalancerListParams{
		ZoneID: cloudflare.String(zoneID),
	})
	if err != nil {
		return nil, fmt.Errorf("listing load balancers: %w", err)
	}
	hostnames := make([]string, 0, len(page.Result))
	for _, lb := range page.Result {
		hostnames = append(hostnames, lb.Name)
	}
	return hostnames, nil
}
