// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/spf13/cobra"
)

func newTestServeCmd() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a test HTTP backend server",
		RunE: func(cmd *cobra.Command, args []string) error {
			podName := os.Getenv("POD_NAME")
			if podName == "" {
				podName = "unknown"
			}

			var counter atomic.Int64

			mux := http.NewServeMux()

			mux.HandleFunc("/_healthz", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			mux.HandleFunc("/_count", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"pod":   podName,
					"count": counter.Load(),
				})
			})

			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				counter.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"pod": podName,
				})
			})

			server := &http.Server{
				Addr:    addr,
				Handler: mux,
			}

			go func() {
				<-cmd.Context().Done()
				_ = server.Close()
			}()

			fmt.Printf("test server listening on %s (pod=%s)\n", addr, podName)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("HTTP server error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "listen address")

	return cmd
}
