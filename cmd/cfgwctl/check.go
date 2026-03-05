// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Run health checks for resources",
	}

	cmd.AddCommand(newCheckGatewayCmd())

	return cmd
}
