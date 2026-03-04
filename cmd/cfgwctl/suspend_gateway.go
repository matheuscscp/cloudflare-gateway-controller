// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func newSuspendGatewayCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "gateway <name>",
		Short: "Suspend reconciliation of a Gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			c, ns, err := buildKubeClient(namespace)
			if err != nil {
				return err
			}

			key := types.NamespacedName{Name: name, Namespace: ns}
			var skipped bool
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var gw gatewayv1.Gateway
				if err := c.Get(ctx, key, &gw); err != nil {
					return err
				}
				if gw.Annotations[apiv1.AnnotationReconcile] == apiv1.AnnotationReconcileDisabled {
					skipped = true
					return nil
				}
				patch := client.MergeFrom(gw.DeepCopy())
				if gw.Annotations == nil {
					gw.Annotations = make(map[string]string)
				}
				gw.Annotations[apiv1.AnnotationReconcile] = apiv1.AnnotationReconcileDisabled
				return c.Patch(ctx, &gw, patch, client.FieldOwner(cliFieldManager))
			}); err != nil {
				return fmt.Errorf("patching Gateway annotation: %w", err)
			}

			if skipped {
				fmt.Printf("Reconciliation is already suspended for Gateway %s/%s\n", ns, name)
			} else {
				fmt.Printf("Suspended reconciliation for Gateway %s/%s\n", ns, name)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace of the Gateway (defaults to kubeconfig context namespace)")

	return cmd
}
