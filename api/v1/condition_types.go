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
)

// Reasons for the Ready condition.
const (
	ReasonReconciled    = "ReconciliationSucceeded"
	ReasonFailed        = "ReconciliationFailed"
	ReasonInvalidParams = "InvalidParameters"
)

// Reasons for the RouteReferenceGrants condition.
const (
	ReasonReferencesAllowed = "ReferencesAllowed"
	ReasonReferencesDenied  = "ReferencesDenied"
)
