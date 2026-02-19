// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newRootCmd() *cobra.Command {
	var credentialsFile string

	cmd := &cobra.Command{
		Use:           "cfgwctl",
		Short:         "CLI for inspecting Cloudflare state managed by cloudflare-gateway-controller",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&credentialsFile, "credentials-file", "",
		"path to credentials file (KEY=VALUE format with CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN)")

	cmd.AddCommand(newTunnelCmd(&credentialsFile))
	cmd.AddCommand(newDNSCmd(&credentialsFile))

	return cmd
}
