// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func newReconcileGatewayCmd() *cobra.Command {
	var namespace string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "gateway <name>",
		Short: "Trigger reconciliation of a Gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			c, ns, err := buildKubeClient(namespace)
			if err != nil {
				return err
			}

			// Snapshot the current status field BEFORE setting the annotation.
			key := types.NamespacedName{Name: name, Namespace: ns}
			oldValue := snapshotCGSField(ctx, c, name, ns,
				func(cgs *apiv1.CloudflareGatewayStatus) string { return cgs.Status.LastHandledReconcileAt })

			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var gw gatewayv1.Gateway
				if err := c.Get(ctx, key, &gw); err != nil {
					return err
				}
				if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.AnnotationReconcileDisabled {
					return fmt.Errorf("reconciliation is suspended for Gateway %s/%s, use 'cfgwctl resume gateway' first", ns, name)
				}
				patch := client.MergeFrom(gw.DeepCopy())
				if gw.Annotations == nil {
					gw.Annotations = make(map[string]string)
				}
				gw.Annotations[apiv1.AnnotationReconcileRequestedAt] = time.Now().Format(time.RFC3339Nano)
				return c.Patch(ctx, &gw, patch, client.FieldOwner(cliFieldManager))
			}); err != nil {
				return err
			}
			fmt.Printf("Requested reconciliation for Gateway %s/%s\n", ns, name)

			if err := waitForReconciliation(ctx, c, name, ns,
				func(cgs *apiv1.CloudflareGatewayStatus) string { return cgs.Status.LastHandledReconcileAt },
				oldValue, timeout); err != nil {
				return err
			}
			fmt.Println("Reconciliation completed")
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace of the Gateway (defaults to kubeconfig context namespace)")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "timeout waiting for reconciliation")

	return cmd
}
