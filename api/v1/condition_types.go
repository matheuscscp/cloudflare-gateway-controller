// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

// Condition type for kstatus compatibility.
const ReadyCondition = "Ready"

// ReadyReason is set when the resource has been successfully reconciled.
const ReadyReason = "ReconciliationSucceeded"

// NotReadyReason is set when the reconciliation has failed.
const NotReadyReason = "ReconciliationFailed"

// InvalidParametersNotReady is set when the GatewayClass parametersRef
// Secret is missing or contains invalid credentials.
const InvalidParametersNotReady = "InvalidParameters"
