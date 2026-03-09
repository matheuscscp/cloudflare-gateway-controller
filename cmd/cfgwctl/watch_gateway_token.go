// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/spf13/cobra"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func newWatchGatewayTokenCmd() *cobra.Command {
	var (
		namespace string
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "token <name>",
		Short: "Watch an ongoing token rotation for a Gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			kc, err := buildKubeClients(namespace)
			if err != nil {
				return err
			}
			c, ns := kc.Client, kc.Namespace
			key := types.NamespacedName{Name: name, Namespace: ns}

			// Read CGS for deployment count and current hash.
			var cgs apiv1.CloudflareGatewayStatus
			if err := c.Get(ctx, key, &cgs); err != nil {
				return fmt.Errorf("getting CloudflareGatewayStatus: %w", err)
			}

			var currentHash string
			if cgs.Status.Tunnel != nil && cgs.Status.Tunnel.Token != nil {
				currentHash = cgs.Status.Tunnel.Token.Hash
			}

			numDeployments := 0
			var pendingNames []string
			var oldHash string
			for _, ref := range cgs.Status.Inventory {
				if ref.APIVersion != apiv1.APIVersionApps || ref.Kind != apiv1.KindDeployment {
					continue
				}
				numDeployments++
				var deploy appsv1.Deployment
				if err := c.Get(ctx, types.NamespacedName{
					Namespace: ns,
					Name:      ref.Name,
				}, &deploy); err != nil {
					return fmt.Errorf("getting Deployment %s: %w", ref.Name, err)
				}
				h := deploy.Spec.Template.Annotations[apiv1.AnnotationTokenHash]
				if h != currentHash {
					pendingNames = append(pendingNames, ref.Name)
					if oldHash == "" {
						oldHash = h
					}
				}
			}

			if numDeployments == 0 {
				return fmt.Errorf("no tunnel Deployments found in Gateway inventory")
			}

			if len(pendingNames) == 0 {
				fmt.Printf("No ongoing token rotation for Gateway %s/%s. All %d deployment(s) are up to date.\n", ns, name, numDeployments)
				return nil
			}

			// Check suspension before starting to watch.
			var gw gatewayv1.Gateway
			if err := c.Get(ctx, key, &gw); err != nil {
				return fmt.Errorf("getting Gateway: %w", err)
			}
			if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.AnnotationReconcileDisabled {
				return fmt.Errorf("gateway %s/%s is suspended with %d/%d deployment(s) pending update, use 'cfgwctl resume gateway %s -n %s' first",
					ns, name, len(pendingNames), numDeployments, name, ns)
			}

			fmt.Printf("Ongoing rotation for Gateway %s/%s: %d/%d deployment(s) pending update.\n",
				ns, name, len(pendingNames), numDeployments)

			// Watch the ongoing rotation.
			clientset, err := kubernetes.NewForConfig(kc.Config)
			if err != nil {
				return fmt.Errorf("creating kubernetes clientset: %w", err)
			}
			dynClient, err := dynamic.NewForConfig(kc.Config)
			if err != nil {
				return fmt.Errorf("creating dynamic client: %w", err)
			}

			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			err = watchRotation(ctx, clientset, dynClient, ns, name, numDeployments, oldHash, nil)
			if err != nil {
				return err
			}

			fmt.Printf("Token rotation completed for Gateway %s/%s\n", ns, name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace of the Gateway (defaults to kubeconfig context namespace)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "timeout waiting for rotation to complete")

	return cmd
}
