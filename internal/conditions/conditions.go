// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package conditions

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Find returns a pointer to the condition with the given type, or nil if not found.
func Find(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// Changed reports whether the desired condition values differ from
// the existing condition of the same type. A condition is considered changed
// when it doesn't exist yet, or when any of Status, Reason, Message, or
// ObservedGeneration differ.
func Changed(existing []metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, generation int64) bool {
	prev := Find(existing, condType)
	if prev == nil {
		return true
	}
	return prev.Status != status || prev.Reason != reason || prev.Message != message || prev.ObservedGeneration != generation
}

// Set replaces the existing condition list with the desired ones, preserving
// LastTransitionTime when the status hasn't changed.
func Set(existing, desired []metav1.Condition) []metav1.Condition {
	result := make([]metav1.Condition, 0, len(desired))
	for _, d := range desired {
		c := d
		if prev := Find(existing, d.Type); prev != nil && prev.Status == d.Status {
			c.LastTransitionTime = prev.LastTransitionTime
		}
		result = append(result, c)
	}
	return result
}

// Upsert upserts a single condition in the list, preserving
// LastTransitionTime when the status hasn't changed and keeping other
// conditions untouched.
func Upsert(existing []metav1.Condition, desired metav1.Condition) []metav1.Condition {
	if prev := Find(existing, desired.Type); prev != nil {
		if prev.Status == desired.Status {
			desired.LastTransitionTime = prev.LastTransitionTime
		}
		*prev = desired
		return existing
	}
	return append(existing, desired)
}

// Remove removes a condition by type. Returns the modified slice.
func Remove(conds []metav1.Condition, condType string) []metav1.Condition {
	for i, c := range conds {
		if c.Type == condType {
			return append(conds[:i], conds[i+1:]...)
		}
	}
	return conds
}
