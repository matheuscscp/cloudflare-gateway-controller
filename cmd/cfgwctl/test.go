// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newTestCmd() *cobra.Command {
	var credentialsFile string

	cmd := &cobra.Command{
		Use:   "test",
		Short: "Test and inspection commands for Cloudflare resources",
	}

	cmd.PersistentFlags().StringVar(&credentialsFile, "credentials-file", "",
		"path to credentials file (KEY=VALUE format with CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN)")

	cmd.AddCommand(newTunnelCmd(&credentialsFile))
	cmd.AddCommand(newDNSCmd(&credentialsFile))
	cmd.AddCommand(newTestServeCmd())
	cmd.AddCommand(newTestLoadCmd())

	return cmd
}
