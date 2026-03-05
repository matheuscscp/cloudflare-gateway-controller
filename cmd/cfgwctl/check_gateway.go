// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func newCheckGatewayCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "gateway <name>",
		Short: "Run health checks for a Gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			c, ns, err := buildKubeClient(namespace)
			if err != nil {
				return err
			}

			// Fetch the Gateway.
			var gw gatewayv1.Gateway
			if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &gw); err != nil {
				return fmt.Errorf("getting Gateway %s/%s: %w", ns, name, err)
			}

			// Resolve the CloudflareGatewayParameters.
			if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
				return fmt.Errorf("gateway %s/%s has no infrastructure.parametersRef", ns, name)
			}
			ref := gw.Spec.Infrastructure.ParametersRef
			if string(ref.Kind) != apiv1.KindCloudflareGatewayParameters || ref.Group != gatewayv1.Group(apiv1.Group) {
				return fmt.Errorf("infrastructure.parametersRef does not reference a CloudflareGatewayParameters")
			}
			var params apiv1.CloudflareGatewayParameters
			if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &params); err != nil {
				return fmt.Errorf("getting CloudflareGatewayParameters %s/%s: %w", ns, ref.Name, err)
			}

			// Extract the health URL.
			if params.Spec.Tunnel == nil || params.Spec.Tunnel.Health == nil || params.Spec.Tunnel.Health.URL == "" {
				return fmt.Errorf("gateway %s/%s has no tunnel.health.url configured", ns, name)
			}
			healthURL := params.Spec.Tunnel.Health.URL

			// Perform the health check.
			fmt.Printf("Checking health URL: %s\n", healthURL)
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(healthURL)
			if err != nil {
				return fmt.Errorf("health check failed: %w", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("health check failed: HTTP %d", resp.StatusCode)
			}

			fmt.Printf("Health check passed: HTTP %d\n", resp.StatusCode)
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace of the Gateway (defaults to kubeconfig context namespace)")

	return cmd
}
