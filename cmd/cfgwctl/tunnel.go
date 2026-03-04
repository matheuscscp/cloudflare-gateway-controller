// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	cfclient "github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/proxy"
)

const serverAddr = ":8080"

func newTunnelCmd() *cobra.Command {
	var namespace string
	var configMapName string

	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Run the tunnel proxy",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTunnel(namespace, configMapName)
		},
	}

	cmd.Flags().StringVar(&namespace, proxy.FlagNamespace, "", "namespace of the Gateway resources (required)")
	cmd.Flags().StringVar(&configMapName, proxy.FlagConfigMapName, "", "name of the route ConfigMap for this Gateway (required)")
	cobra.CheckErr(cmd.MarkFlagRequired(proxy.FlagNamespace))
	cobra.CheckErr(cmd.MarkFlagRequired(proxy.FlagConfigMapName))

	return cmd
}

func runTunnel(namespace, configMapName string) error {
	ctx := ctrl.SetupSignalHandler()

	p := &proxy.Proxy{}

	// Initialize Kubernetes client.
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("creating in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	// Start ConfigMap informer and wait for initial cache sync.
	configMapWatcher := proxy.NewConfigMapWatcher(clientset, namespace, configMapName, proxy.RouteConfigMapKey, p)
	configMapWatcher.Start(ctx.Done())

	graceShutdownC := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(graceShutdownC)
	}()

	// RunTunnel blocks until the context is cancelled.
	token := os.Getenv(cfclient.TunnelTokenSecretKey)
	if token == "" {
		return fmt.Errorf("environment variable %s is required", cfclient.TunnelTokenSecretKey)
	}
	if err := cfclient.RunTunnel(ctx, p, p, serverAddr, p.ConfigLoaded, token, graceShutdownC); err != nil {
		return fmt.Errorf("tunnel error: %w", err)
	}
	return nil
}
