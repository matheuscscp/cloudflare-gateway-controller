// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

// Condition types.
const (
	// ConditionReady is a kstatus-compatible condition type indicating
	// whether the resource has been successfully reconciled.
	ConditionReady = "Ready"

	// ConditionRouteReferenceGrants is a Gateway condition that reports
	// HTTPRoutes denied due to missing or failed cross-namespace
	// ReferenceGrant checks.
	ConditionRouteReferenceGrants = "RouteReferenceGrants"

	// ConditionBackendReferenceGrants is a Gateway condition that reports
	// HTTPRoute backendRefs denied due to missing or failed cross-namespace
	// ReferenceGrant checks.
	ConditionBackendReferenceGrants = "BackendReferenceGrants"

	// ConditionDNSManagement is a Gateway condition that reports the state
	// of DNS CNAME record management for the Gateway's zone.
	ConditionDNSManagement = "DNSManagement"
)

// Reasons for the Ready condition.
const (
	ReasonReconciliationSucceeded = "ReconciliationSucceeded"
	ReasonReconciliationDisabled  = "ReconciliationDisabled"
	ReasonReconciliationFailed    = "ReconciliationFailed"
	ReasonProgressingWithRetry    = "ProgressingWithRetry"
	ReasonInvalidParameters       = "InvalidParameters"
)

// Reasons for the RouteReferenceGrants condition.
const (
	ReasonReferencesAllowed = "ReferencesAllowed"
	ReasonReferencesDenied  = "ReferencesDenied"
)

// Reasons for the DNSManagement condition.
const (
	ReasonDNSReconciled = "Reconciled"
	ReasonDNSNotEnabled = "NotEnabled"
)
