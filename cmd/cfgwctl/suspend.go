// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newSuspendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "suspend",
		Short: "Suspend reconciliation of resources",
	}

	cmd.AddCommand(newSuspendGatewayCmd())

	return cmd
}
