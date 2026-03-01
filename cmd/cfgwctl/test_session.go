// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"

	"github.com/spf13/cobra"
)

func newTestSessionCmd() *cobra.Command {
	var (
		url        string
		requests   int
		cookieName string
		hostname   string
	)

	cmd := &cobra.Command{
		Use:   "session",
		Short: "Test cookie-based session persistence",
		RunE: func(cmd *cobra.Command, args []string) error {
			jar, err := cookiejar.New(nil)
			if err != nil {
				return fmt.Errorf("creating cookie jar: %w", err)
			}

			client := &http.Client{Jar: jar}

			// First request — should get Set-Cookie.
			fmt.Printf("Sending initial request to %s...\n", url)
			resp, err := client.Get(url)
			if err != nil {
				return fmt.Errorf("initial request: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("initial request returned status %d", resp.StatusCode)
			}

			var firstResult struct {
				Pod  string `json:"pod"`
				Host string `json:"host"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&firstResult); err != nil {
				return fmt.Errorf("decoding initial response: %w", err)
			}

			if firstResult.Pod == "" {
				return fmt.Errorf("initial response missing 'pod' field")
			}

			// Verify the Host header was forwarded correctly.
			if hostname != "" && firstResult.Host != hostname {
				return fmt.Errorf("expected host %q, got %q", hostname, firstResult.Host)
			}

			// Check we got a session cookie.
			parsedURL, err := resp.Request.URL.Parse(url)
			if err != nil {
				return fmt.Errorf("parsing URL: %w", err)
			}
			cookies := jar.Cookies(parsedURL)
			var sessionCookie *http.Cookie
			for _, c := range cookies {
				if c.Name == cookieName {
					sessionCookie = c
					break
				}
			}
			if sessionCookie == nil {
				return fmt.Errorf("no %q cookie in response", cookieName)
			}

			fmt.Printf("Pinned to pod %q (cookie: %s=%s)\n", firstResult.Pod, cookieName, sessionCookie.Value)

			// Follow-up requests with cookie — all should go to the same pod.
			fmt.Printf("Sending %d follow-up requests...\n", requests)
			for i := range requests {
				resp, err := client.Get(url)
				if err != nil {
					return fmt.Errorf("request %d: %w", i+1, err)
				}

				var result struct {
					Pod  string `json:"pod"`
					Host string `json:"host"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
					_ = resp.Body.Close()
					return fmt.Errorf("request %d: decoding response: %w", i+1, err)
				}
				_ = resp.Body.Close()

				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return fmt.Errorf("request %d: status %d", i+1, resp.StatusCode)
				}

				if result.Pod != firstResult.Pod {
					return fmt.Errorf("request %d: expected pod %q, got %q (session affinity broken)",
						i+1, firstResult.Pod, result.Pod)
				}

				// Verify the Host header was forwarded correctly.
				if hostname != "" && result.Host != hostname {
					return fmt.Errorf("request %d: expected host %q, got %q", i+1, hostname, result.Host)
				}
			}

			fmt.Printf("\nSession persistence check PASSED: all %d requests went to pod %q\n",
				requests+1, firstResult.Pod)
			return nil
		},
	}

	cmd.Flags().StringVar(&url, "url", "", "target URL (required)")
	cmd.Flags().IntVar(&requests, "requests", 50, "number of follow-up requests")
	cmd.Flags().StringVar(&cookieName, "cookie-name", "cgw-session", "session cookie name")
	cmd.Flags().StringVar(&hostname, "hostname", "", "expected Host header value (optional)")
	cobra.CheckErr(cmd.MarkFlagRequired("url"))

	return cmd
}
