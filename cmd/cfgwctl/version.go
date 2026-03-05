// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print CLI and controller version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cfgwctl version %s\n", VERSION)

			c, _, err := buildKubeClient("")
			if err != nil {
				return nil
			}

			var deployments appsv1.DeploymentList
			if err := c.List(context.Background(), &deployments, client.MatchingLabels{
				apiv1.LabelComponent: apiv1.LabelComponentController,
			}); err != nil {
				return nil
			}

			if len(deployments.Items) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "controller: not found")
				return nil
			}

			for _, d := range deployments.Items {
				image := ""
				if len(d.Spec.Template.Spec.Containers) > 0 {
					image = d.Spec.Template.Spec.Containers[0].Image
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "controller: %s (%s/%s/%s)\n", image, apiv1.KindDeployment, d.Namespace, d.Name)
			}

			return nil
		},
	}
}
