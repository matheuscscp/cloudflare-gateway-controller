// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
