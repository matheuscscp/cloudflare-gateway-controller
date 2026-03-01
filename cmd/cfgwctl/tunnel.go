// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newTunnelCmd(credentialsFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Manage Cloudflare tunnels",
	}
	cmd.AddCommand(
		newTunnelListCmd(credentialsFile),
		newTunnelGetIDCmd(credentialsFile),
		newTunnelDeleteCmd(credentialsFile),
		newTunnelCleanupConnectionsCmd(credentialsFile),
	)
	return cmd
}

func newTunnelListCmd(credentialsFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all active Cloudflare tunnels",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			tunnels, err := c.ListTunnels(cmd.Context())
			if err != nil {
				return err
			}
			type tunnelOutput struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			out := make([]tunnelOutput, len(tunnels))
			for i, t := range tunnels {
				out[i] = tunnelOutput{ID: t.ID, Name: t.Name}
			}
			return printJSON(out)
		},
	}
	return cmd
}

func newTunnelGetIDCmd(credentialsFile *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "get-id",
		Short: "Look up a tunnel ID by name",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			tunnelID, err := c.GetTunnelIDByName(cmd.Context(), name)
			if err != nil {
				return err
			}
			return printJSON(map[string]string{"tunnelId": tunnelID})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "tunnel name")
	cobra.CheckErr(cmd.MarkFlagRequired("name"))
	return cmd
}

func newTunnelDeleteCmd(credentialsFile *string) *cobra.Command {
	var tunnelID string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a Cloudflare tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.DeleteTunnel(cmd.Context(), tunnelID)
		},
	}
	cmd.Flags().StringVar(&tunnelID, "tunnel-id", "", "tunnel ID")
	cobra.CheckErr(cmd.MarkFlagRequired("tunnel-id"))
	return cmd
}

func newTunnelCleanupConnectionsCmd(credentialsFile *string) *cobra.Command {
	var tunnelID string
	cmd := &cobra.Command{
		Use:   "cleanup-connections",
		Short: "Clean up stale connections for a tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.CleanupTunnelConnections(cmd.Context(), tunnelID)
		},
	}
	cmd.Flags().StringVar(&tunnelID, "tunnel-id", "", "tunnel ID")
	cobra.CheckErr(cmd.MarkFlagRequired("tunnel-id"))
	return cmd
}
