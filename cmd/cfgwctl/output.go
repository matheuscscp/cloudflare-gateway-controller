// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"encoding/json"
	"os"
)

// ingressRuleOutput provides clean camelCase JSON serialization for ingress rules.
type ingressRuleOutput struct {
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
	Path     string `json:"path,omitempty"`
}

// printJSON writes v as indented JSON to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
