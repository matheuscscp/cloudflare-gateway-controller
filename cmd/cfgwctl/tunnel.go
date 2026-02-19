// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

func newTunnelCmd(credentialsFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Manage Cloudflare tunnels",
	}
	cmd.AddCommand(
		newTunnelCreateCmd(credentialsFile),
		newTunnelGetIDCmd(credentialsFile),
		newTunnelDeleteCmd(credentialsFile),
		newTunnelGetTokenCmd(credentialsFile),
		newTunnelGetConfigCmd(credentialsFile),
		newTunnelUpdateConfigCmd(credentialsFile),
	)
	return cmd
}

func newTunnelCreateCmd(credentialsFile *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new Cloudflare tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			tunnelID, err := c.CreateTunnel(cmd.Context(), name)
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

func newTunnelGetTokenCmd(credentialsFile *string) *cobra.Command {
	var tunnelID string
	cmd := &cobra.Command{
		Use:   "get-token",
		Short: "Get a tunnel's connection token",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			token, err := c.GetTunnelToken(cmd.Context(), tunnelID)
			if err != nil {
				return err
			}
			return printJSON(map[string]string{"token": token})
		},
	}
	cmd.Flags().StringVar(&tunnelID, "tunnel-id", "", "tunnel ID")
	cobra.CheckErr(cmd.MarkFlagRequired("tunnel-id"))
	return cmd
}

func newTunnelGetConfigCmd(credentialsFile *string) *cobra.Command {
	var tunnelID string
	cmd := &cobra.Command{
		Use:   "get-config",
		Short: "Get a tunnel's ingress configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			rules, err := c.GetTunnelConfiguration(cmd.Context(), tunnelID)
			if err != nil {
				return err
			}
			out := make([]ingressRuleOutput, len(rules))
			for i, r := range rules {
				out[i] = ingressRuleOutput{
					Hostname: r.Hostname,
					Service:  r.Service,
					Path:     r.Path,
				}
			}
			return printJSON(out)
		},
	}
	cmd.Flags().StringVar(&tunnelID, "tunnel-id", "", "tunnel ID")
	cobra.CheckErr(cmd.MarkFlagRequired("tunnel-id"))
	return cmd
}

func newTunnelUpdateConfigCmd(credentialsFile *string) *cobra.Command {
	var tunnelID, ingressJSON string
	cmd := &cobra.Command{
		Use:   "update-config",
		Short: "Update a tunnel's ingress configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			var rules []ingressRuleOutput
			if err := json.Unmarshal([]byte(ingressJSON), &rules); err != nil {
				return fmt.Errorf("parsing --ingress JSON: %w", err)
			}
			ingress := make([]cloudflare.IngressRule, len(rules))
			for i, r := range rules {
				ingress[i] = cloudflare.IngressRule{
					Hostname: r.Hostname,
					Service:  r.Service,
					Path:     r.Path,
				}
			}
			return c.UpdateTunnelConfiguration(cmd.Context(), tunnelID, ingress)
		},
	}
	cmd.Flags().StringVar(&tunnelID, "tunnel-id", "", "tunnel ID")
	cmd.Flags().StringVar(&ingressJSON, "ingress", "",
		`ingress rules as JSON array, e.g. '[{"hostname":"app.example.com","service":"http://localhost:8080"}]'`)
	cobra.CheckErr(cmd.MarkFlagRequired("tunnel-id"))
	cobra.CheckErr(cmd.MarkFlagRequired("ingress"))
	return cmd
}
