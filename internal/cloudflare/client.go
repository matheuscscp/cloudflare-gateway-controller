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

// Client abstracts Cloudflare tunnel operations.
type Client interface {
	CreateTunnel(ctx context.Context, name string) (tunnelID string, err error)
	GetTunnelIDByName(ctx context.Context, name string) (tunnelID string, err error)
	CleanupTunnelConnections(ctx context.Context, tunnelID string) error
	DeleteTunnel(ctx context.Context, tunnelID string) error
	GetTunnelToken(ctx context.Context, tunnelID string) (token string, err error)
	GetTunnelConfiguration(ctx context.Context, tunnelID string) ([]IngressRule, error)
	UpdateTunnelConfiguration(ctx context.Context, tunnelID string, ingress []IngressRule) error
	ListZoneIDs(ctx context.Context) ([]string, error)
	FindZoneIDByHostname(ctx context.Context, hostname string) (string, error)
	EnsureDNSCNAME(ctx context.Context, zoneID, hostname, target string) error
	DeleteDNSCNAME(ctx context.Context, zoneID, hostname string) error
	ListDNSCNAMEsByTarget(ctx context.Context, zoneID, target string) ([]string, error)
}

// IsConflict reports whether the error is a 409 Conflict from the Cloudflare API.
func IsConflict(err error) bool {
	var apiErr *cloudflare.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
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
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
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

func (c *client) EnsureDNSCNAME(ctx context.Context, zoneID, hostname, target string) error {
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
