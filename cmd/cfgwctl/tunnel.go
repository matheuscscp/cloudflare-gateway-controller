// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	cfclient "github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/proxy"
)

type tunnelOptions struct {
	namespace     string
	configMapName string
	configMapKey  string
	addr          string
	healthAddr    string
	metricsAddr   string
}

func newTunnelCmd() *cobra.Command {
	opts := &tunnelOptions{}

	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Run the tunnel proxy",
		RunE:  opts.run,
	}

	cmd.Flags().StringVar(&opts.namespace, "namespace", "", "namespace of the ConfigMap (required)")
	cmd.Flags().StringVar(&opts.configMapName, "configmap-name", "", "name of the ConfigMap (required)")
	cmd.Flags().StringVar(&opts.configMapKey, "configmap-key", "config.yaml", "key in the ConfigMap")
	cmd.Flags().StringVar(&opts.addr, "addr", "127.0.0.1:8080", "listen address")
	cmd.Flags().StringVar(&opts.healthAddr, "health-addr", ":8081", "health check listen address")
	cmd.Flags().StringVar(&opts.metricsAddr, "metrics-addr", "0.0.0.0:2000", "cloudflared metrics listen address")
	cobra.CheckErr(cmd.MarkFlagRequired("namespace"))
	cobra.CheckErr(cmd.MarkFlagRequired("configmap-name"))

	return cmd
}

func (o *tunnelOptions) run(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("creating in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	p := &proxy.Proxy{}
	watcher := proxy.NewWatcher(clientset, o.namespace, o.configMapName, o.configMapKey, p)

	graceShutdownC := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(graceShutdownC)
	}()

	go func() {
		if err := cfclient.RunTunnel(ctx, p, p, o.metricsAddr, graceShutdownC); err != nil {
			fmt.Printf("tunnel error: %v\n", err)
			os.Exit(1)
		}
	}()

	stopCh := ctx.Done()
	watcher.Start(stopCh)

	server := &http.Server{
		Addr:    o.addr,
		Handler: p,
	}

	proxyURL := fmt.Sprintf("http://%s/", o.addr)
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		resp, err := http.Get(proxyURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("proxy unreachable: %v", err), http.StatusServiceUnavailable)
			return
		}
		_ = resp.Body.Close()
		if resp.Header.Get(proxy.HeaderConfigNotLoaded) != "" {
			http.Error(w, "proxy config not loaded yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	healthServer := &http.Server{
		Addr:    o.healthAddr,
		Handler: healthMux,
	}

	go func() {
		<-stopCh
		_ = server.Close()
		_ = healthServer.Close()
	}()

	go func() {
		fmt.Printf("tunnel health check listening on %s\n", o.healthAddr)
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("health server error: %v\n", err)
		}
	}()

	fmt.Printf("tunnel proxy listening on %s\n", o.addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}
	return nil
}
