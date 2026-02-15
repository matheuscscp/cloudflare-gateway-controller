// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

// Condition type for kstatus compatibility.
const ReadyCondition = "Ready"

// Condition reasons.
const (
	ReadyReason               = "ReconciliationSucceeded"
	NotReadyReason            = "ReconciliationFailed"
	InvalidParametersNotReady = "InvalidParameters"
)
