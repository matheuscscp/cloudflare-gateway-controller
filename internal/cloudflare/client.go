// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare

import (
	"context"
	"errors"
	"net/http"

	cloudflare "github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
)

// ClientConfig holds the credentials needed to interact with the Cloudflare API.
type ClientConfig struct {
	APIToken  string
	AccountID string
}

// TunnelClient abstracts Cloudflare tunnel operations.
type TunnelClient interface {
	CreateTunnel(ctx context.Context, name string) (tunnelID string, err error)
	UpdateTunnel(ctx context.Context, tunnelID, name string) error
	DeleteTunnel(ctx context.Context, tunnelID string) error
	GetTunnelToken(ctx context.Context, tunnelID string) (token string, err error)
}

// TunnelClientFactory creates a TunnelClient from a ClientConfig.
type TunnelClientFactory func(cfg ClientConfig) (TunnelClient, error)

// NewTunnelClient creates a new TunnelClient backed by the Cloudflare API.
func NewTunnelClient(cfg ClientConfig) (TunnelClient, error) {
	client := cloudflare.NewClient(option.WithAPIToken(cfg.APIToken))
	return &tunnelClient{
		client:    client,
		accountID: cfg.AccountID,
	}, nil
}

type tunnelClient struct {
	client    *cloudflare.Client
	accountID string
}

func (c *tunnelClient) CreateTunnel(ctx context.Context, name string) (string, error) {
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

func (c *tunnelClient) UpdateTunnel(ctx context.Context, tunnelID, name string) error {
	_, err := c.client.ZeroTrust.Tunnels.Cloudflared.Edit(ctx, tunnelID, zero_trust.TunnelCloudflaredEditParams{
		AccountID: cloudflare.String(c.accountID),
		Name:      cloudflare.F(name),
	})
	return err
}

func (c *tunnelClient) DeleteTunnel(ctx context.Context, tunnelID string) error {
	_, err := c.client.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredDeleteParams{
		AccountID: cloudflare.String(c.accountID),
	})
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *tunnelClient) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	token, err := c.client.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.String(c.accountID),
	})
	if err != nil {
		return "", err
	}
	return *token, nil
}
