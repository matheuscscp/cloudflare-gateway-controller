// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newDNSCmd(credentialsFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Manage Cloudflare DNS records",
	}
	cmd.AddCommand(
		newDNSListZonesCmd(credentialsFile),
		newDNSFindZoneCmd(credentialsFile),
		newDNSEnsureCNAMECmd(credentialsFile),
		newDNSDeleteCNAMECmd(credentialsFile),
		newDNSListCNAMEsCmd(credentialsFile),
	)
	return cmd
}

func newDNSListZonesCmd(credentialsFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list-zones",
		Short: "List all zone IDs in the account",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			zoneIDs, err := c.ListZoneIDs(cmd.Context())
			if err != nil {
				return err
			}
			return printJSON(map[string][]string{"zoneIds": zoneIDs})
		},
	}
}

func newDNSFindZoneCmd(credentialsFile *string) *cobra.Command {
	var hostname string
	cmd := &cobra.Command{
		Use:   "find-zone",
		Short: "Find a zone ID by hostname",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			zoneID, err := c.FindZoneIDByHostname(cmd.Context(), hostname)
			if err != nil {
				return err
			}
			return printJSON(map[string]string{"zoneId": zoneID})
		},
	}
	cmd.Flags().StringVar(&hostname, "hostname", "", "hostname to look up")
	cobra.CheckErr(cmd.MarkFlagRequired("hostname"))
	return cmd
}

func newDNSEnsureCNAMECmd(credentialsFile *string) *cobra.Command {
	var zoneID, hostname, target string
	cmd := &cobra.Command{
		Use:   "ensure-cname",
		Short: "Ensure a DNS CNAME record exists",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.EnsureDNSCNAME(cmd.Context(), zoneID, hostname, target)
		},
	}
	cmd.Flags().StringVar(&zoneID, "zone-id", "", "zone ID")
	cmd.Flags().StringVar(&hostname, "hostname", "", "CNAME hostname")
	cmd.Flags().StringVar(&target, "target", "", "CNAME target")
	cobra.CheckErr(cmd.MarkFlagRequired("zone-id"))
	cobra.CheckErr(cmd.MarkFlagRequired("hostname"))
	cobra.CheckErr(cmd.MarkFlagRequired("target"))
	return cmd
}

func newDNSDeleteCNAMECmd(credentialsFile *string) *cobra.Command {
	var zoneID, hostname string
	cmd := &cobra.Command{
		Use:   "delete-cname",
		Short: "Delete a DNS CNAME record",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.DeleteDNSCNAME(cmd.Context(), zoneID, hostname)
		},
	}
	cmd.Flags().StringVar(&zoneID, "zone-id", "", "zone ID")
	cmd.Flags().StringVar(&hostname, "hostname", "", "CNAME hostname to delete")
	cobra.CheckErr(cmd.MarkFlagRequired("zone-id"))
	cobra.CheckErr(cmd.MarkFlagRequired("hostname"))
	return cmd
}

func newDNSListCNAMEsCmd(credentialsFile *string) *cobra.Command {
	var zoneID, target string
	cmd := &cobra.Command{
		Use:   "list-cnames",
		Short: "List DNS CNAME records pointing to a target",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			hostnames, err := c.ListDNSCNAMEsByTarget(cmd.Context(), zoneID, target)
			if err != nil {
				return err
			}
			return printJSON(map[string][]string{"hostnames": hostnames})
		},
	}
	cmd.Flags().StringVar(&zoneID, "zone-id", "", "zone ID")
	cmd.Flags().StringVar(&target, "target", "", "CNAME target to filter by")
	cobra.CheckErr(cmd.MarkFlagRequired("zone-id"))
	cobra.CheckErr(cmd.MarkFlagRequired("target"))
	return cmd
}
