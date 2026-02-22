// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import "github.com/spf13/cobra"

func newLBCmd(credentialsFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lb",
		Short: "Manage Cloudflare Load Balancer resources",
	}
	cmd.AddCommand(
		newLBGetMonitorCmd(credentialsFile),
		newLBGetPoolCmd(credentialsFile),
		newLBListPoolsCmd(credentialsFile),
		newLBListHostnamesCmd(credentialsFile),
		newLBDeleteMonitorCmd(credentialsFile),
		newLBDeletePoolCmd(credentialsFile),
		newLBDeleteCmd(credentialsFile),
	)
	return cmd
}

func newLBGetMonitorCmd(credentialsFile *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "get-monitor",
		Short: "Look up a load balancer monitor by name",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			monitorID, err := c.GetMonitorByName(cmd.Context(), name)
			if err != nil {
				return err
			}
			return printJSON(map[string]string{"monitorId": monitorID})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "monitor name")
	cobra.CheckErr(cmd.MarkFlagRequired("name"))
	return cmd
}

func newLBGetPoolCmd(credentialsFile *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "get-pool",
		Short: "Look up a load balancer pool by name",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			poolID, pool, err := c.GetPoolByName(cmd.Context(), name)
			if err != nil {
				return err
			}
			if poolID == "" {
				return printJSON(map[string]string{"poolId": ""})
			}
			type originOutput struct {
				Name    string  `json:"name"`
				Address string  `json:"address"`
				Enabled bool    `json:"enabled"`
				Weight  float64 `json:"weight"`
			}
			origins := make([]originOutput, len(pool.Origins))
			for i, o := range pool.Origins {
				origins[i] = originOutput{
					Name:    o.Name,
					Address: o.Address,
					Enabled: o.Enabled,
					Weight:  o.Weight,
				}
			}
			return printJSON(struct {
				PoolID    string         `json:"poolId"`
				Name      string         `json:"name"`
				MonitorID string         `json:"monitorId"`
				Origins   []originOutput `json:"origins"`
				Weight    float64        `json:"weight"`
				Enabled   bool           `json:"enabled"`
			}{
				PoolID:    poolID,
				Name:      pool.Name,
				MonitorID: pool.MonitorID,
				Origins:   origins,
				Weight:    pool.Weight,
				Enabled:   pool.Enabled,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "pool name")
	cobra.CheckErr(cmd.MarkFlagRequired("name"))
	return cmd
}

func newLBListPoolsCmd(credentialsFile *string) *cobra.Command {
	var prefix string
	cmd := &cobra.Command{
		Use:   "list-pools",
		Short: "List load balancer pools by name prefix",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			pools, err := c.ListPoolsByPrefix(cmd.Context(), prefix)
			if err != nil {
				return err
			}
			type poolOutput struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			out := make([]poolOutput, len(pools))
			for i, p := range pools {
				out[i] = poolOutput{ID: p.ID, Name: p.Name}
			}
			return printJSON(out)
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "pool name prefix")
	cobra.CheckErr(cmd.MarkFlagRequired("prefix"))
	return cmd
}

func newLBListHostnamesCmd(credentialsFile *string) *cobra.Command {
	var zoneID string
	cmd := &cobra.Command{
		Use:   "list-hostnames",
		Short: "List load balancer hostnames in a zone",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			hostnames, err := c.ListLoadBalancerHostnames(cmd.Context(), zoneID)
			if err != nil {
				return err
			}
			return printJSON(map[string][]string{"hostnames": hostnames})
		},
	}
	cmd.Flags().StringVar(&zoneID, "zone-id", "", "zone ID")
	cobra.CheckErr(cmd.MarkFlagRequired("zone-id"))
	return cmd
}

func newLBDeleteMonitorCmd(credentialsFile *string) *cobra.Command {
	var monitorID string
	cmd := &cobra.Command{
		Use:   "delete-monitor",
		Short: "Delete a load balancer monitor",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.DeleteMonitor(cmd.Context(), monitorID)
		},
	}
	cmd.Flags().StringVar(&monitorID, "monitor-id", "", "monitor ID")
	cobra.CheckErr(cmd.MarkFlagRequired("monitor-id"))
	return cmd
}

func newLBDeletePoolCmd(credentialsFile *string) *cobra.Command {
	var poolID string
	cmd := &cobra.Command{
		Use:   "delete-pool",
		Short: "Delete a load balancer pool",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.DeletePool(cmd.Context(), poolID)
		},
	}
	cmd.Flags().StringVar(&poolID, "pool-id", "", "pool ID")
	cobra.CheckErr(cmd.MarkFlagRequired("pool-id"))
	return cmd
}

func newLBDeleteCmd(credentialsFile *string) *cobra.Command {
	var zoneID, hostname string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a load balancer by hostname",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*credentialsFile)
			if err != nil {
				return err
			}
			return c.DeleteLoadBalancer(cmd.Context(), zoneID, hostname)
		},
	}
	cmd.Flags().StringVar(&zoneID, "zone-id", "", "zone ID")
	cmd.Flags().StringVar(&hostname, "hostname", "", "load balancer hostname")
	cobra.CheckErr(cmd.MarkFlagRequired("zone-id"))
	cobra.CheckErr(cmd.MarkFlagRequired("hostname"))
	return cmd
}
