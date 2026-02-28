// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "cfgwctl",
		Short:         "CLI for cloudflare-gateway-controller",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newControllerCmd())
	cmd.AddCommand(newTestCmd())

	return cmd
}
