// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/sidecar"
)

func newSidecarCmd() *cobra.Command {
	var (
		namespace     string
		configMapName string
		configMapKey  string
		addr          string
		healthAddr    string
	)

	cmd := &cobra.Command{
		Use:   "sidecar",
		Short: "Run the sidecar reverse proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg, err := rest.InClusterConfig()
			if err != nil {
				return fmt.Errorf("creating in-cluster config: %w", err)
			}

			clientset, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("creating kubernetes clientset: %w", err)
			}

			proxy := &sidecar.Proxy{}
			watcher := sidecar.NewWatcher(clientset, namespace, configMapName, configMapKey, proxy)

			stopCh := ctx.Done()
			watcher.Start(stopCh)

			server := &http.Server{
				Addr:    addr,
				Handler: proxy,
			}

			proxyURL := fmt.Sprintf("http://%s/", addr)
			healthMux := http.NewServeMux()
			healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				resp, err := http.Get(proxyURL)
				if err != nil {
					http.Error(w, fmt.Sprintf("proxy unreachable: %v", err), http.StatusServiceUnavailable)
					return
				}
				_ = resp.Body.Close()
				if resp.Header.Get(sidecar.HeaderConfigNotLoaded) != "" {
					http.Error(w, "proxy config not loaded yet", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			healthServer := &http.Server{
				Addr:    healthAddr,
				Handler: healthMux,
			}

			go func() {
				<-stopCh
				_ = server.Close()
				_ = healthServer.Close()
			}()

			go func() {
				fmt.Printf("sidecar health check listening on %s\n", healthAddr)
				if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					fmt.Printf("health server error: %v\n", err)
				}
			}()

			fmt.Printf("sidecar proxy listening on %s\n", addr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("HTTP server error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace of the ConfigMap (required)")
	cmd.Flags().StringVar(&configMapName, "configmap-name", "", "name of the ConfigMap (required)")
	cmd.Flags().StringVar(&configMapKey, "configmap-key", "config.yaml", "key in the ConfigMap")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	cmd.Flags().StringVar(&healthAddr, "health-addr", ":8081", "health check listen address")
	cobra.CheckErr(cmd.MarkFlagRequired("namespace"))
	cobra.CheckErr(cmd.MarkFlagRequired("configmap-name"))

	return cmd
}
