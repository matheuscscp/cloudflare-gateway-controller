// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

// VERSION is set by GoReleaser at build time.
var VERSION = "0.0.0-dev.0"

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cfgwctl",
		Short: "CLI for cloudflare-gateway-controller",

		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newControllerCmd())
	cmd.AddCommand(newTunnelCmd())
	cmd.AddCommand(newTestCmd())
	cmd.AddCommand(newReconcileCmd())
	cmd.AddCommand(newSuspendCmd())
	cmd.AddCommand(newResumeCmd())
	cmd.AddCommand(newRotateCmd())
	cmd.AddCommand(newWatchCmd())

	return cmd
}
