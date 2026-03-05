// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newRotateGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Rotate credentials for a Gateway",
	}

	cmd.AddCommand(newRotateGatewayTokenCmd())

	return cmd
}
