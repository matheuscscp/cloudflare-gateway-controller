// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare

// WithRetryMaxRetries creates a retryClient with the given maxRetries for testing.
func WithRetryMaxRetries(c Client, maxRetries int) Client {
	return &retryClient{inner: c, maxRetries: maxRetries}
}
