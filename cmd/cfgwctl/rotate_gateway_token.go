// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/spf13/cobra"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func newRotateGatewayTokenCmd() *cobra.Command {
	var (
		namespace string
		watch     bool
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "token <name>",
		Short: "Rotate the tunnel token for a Gateway",
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
			numDeployments := 0
			for _, ref := range cgs.Status.Inventory {
				if ref.APIVersion == apiv1.APIVersionApps && ref.Kind == apiv1.KindDeployment {
					numDeployments++
				}
			}
			if numDeployments == 0 {
				return fmt.Errorf("no tunnel Deployments found in Gateway inventory")
			}

			// preflightAndPatch checks suspension and patches the rotation
			// annotation inside RetryOnConflict so both operations see the
			// same Gateway revision.
			preflightAndPatch := func() error {
				return retry.RetryOnConflict(retry.DefaultRetry, func() error {
					var gw gatewayv1.Gateway
					if err := c.Get(ctx, key, &gw); err != nil {
						return fmt.Errorf("getting Gateway: %w", err)
					}
					if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.AnnotationReconcileDisabled {
						return fmt.Errorf("reconciliation is suspended for Gateway %s/%s, use 'cfgwctl resume gateway' first", ns, name)
					}
					patch := client.MergeFrom(gw.DeepCopy())
					if gw.Annotations == nil {
						gw.Annotations = make(map[string]string)
					}
					gw.Annotations[apiv1.AnnotationRotateTokenRequestedAt] = time.Now().Format(time.RFC3339Nano)
					return c.Patch(ctx, &gw, patch, client.FieldOwner(cliFieldManager))
				})
			}

			if !watch {
				if err := preflightAndPatch(); err != nil {
					return err
				}
				fmt.Printf("Requested token rotation for Gateway %s/%s\n", ns, name)
				return nil
			}

			// Watch mode: patch inside watchRotation after informer sync.
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

			var currentHash string
			if cgs.Status.Tunnel != nil && cgs.Status.Tunnel.Token != nil {
				currentHash = cgs.Status.Tunnel.Token.Hash
			}
			err = watchRotation(ctx, clientset, dynClient, ns, name, numDeployments, currentHash, preflightAndPatch)
			if err != nil {
				if _, ok := errors.AsType[*watchSuspendedError](err); ok {
					fmt.Printf("\nHint: use 'cfgwctl resume gateway %s -n %s' to resume, then 'cfgwctl watch gateway token %s -n %s' to continue watching.\n", name, ns, name, ns)
				} else if ctx.Err() != nil {
					fmt.Printf("\nHint: use 'cfgwctl watch gateway token %s -n %s' to continue watching the rollout.\n", name, ns)
				}
				return err
			}

			fmt.Printf("Token rotation completed for Gateway %s/%s\n", ns, name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace of the Gateway (defaults to kubeconfig context namespace)")
	cmd.Flags().BoolVar(&watch, "watch", false, "watch the rolling update in real-time until completion")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "timeout waiting for token rotation (only used with --watch)")

	return cmd
}
