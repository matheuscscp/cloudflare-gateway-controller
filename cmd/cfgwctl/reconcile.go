// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Trigger reconciliation of resources",
	}

	cmd.AddCommand(newReconcileGatewayCmd())

	return cmd
}
