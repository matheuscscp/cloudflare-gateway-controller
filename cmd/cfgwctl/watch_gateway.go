// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newWatchGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Watch ongoing operations for a Gateway",
	}

	cmd.AddCommand(newWatchGatewayTokenCmd())

	return cmd
}
