// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// loadCredentials reads Cloudflare credentials from a KEY=VALUE file (if
// specified) with fallback to environment variables. Both CLOUDFLARE_ACCOUNT_ID
// and CLOUDFLARE_API_TOKEN must be present.
func loadCredentials(credentialsFile string) (cloudflare.ClientConfig, error) {
	values := make(map[string]string)
	if credentialsFile != "" {
		data, err := os.ReadFile(credentialsFile)
		if err != nil {
			return cloudflare.ClientConfig{}, fmt.Errorf("reading credentials file: %w", err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}

	accountID := values["CLOUDFLARE_ACCOUNT_ID"]
	if accountID == "" {
		accountID = os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	}
	apiToken := values["CLOUDFLARE_API_TOKEN"]
	if apiToken == "" {
		apiToken = os.Getenv("CLOUDFLARE_API_TOKEN")
	}

	if accountID == "" || apiToken == "" {
		return cloudflare.ClientConfig{}, fmt.Errorf(
			"CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN must be provided via --credentials-file or environment variables")
	}

	return cloudflare.ClientConfig{
		APIToken:  apiToken,
		AccountID: accountID,
	}, nil
}

// newClient loads credentials and creates a Cloudflare API client.
func newClient(credentialsFile string) (cloudflare.Client, error) {
	cfg, err := loadCredentials(credentialsFile)
	if err != nil {
		return nil, err
	}
	return cloudflare.NewClient(cfg)
}
