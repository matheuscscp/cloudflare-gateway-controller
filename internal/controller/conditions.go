// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// conditionChanged reports whether the desired condition values differ from
// the existing condition of the same type. A condition is considered changed
// when it doesn't exist yet, or when any of Status, Reason, Message, or
// ObservedGeneration differ.
func conditionChanged(existing []metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, generation int64) bool {
	prev := findCondition(existing, condType)
	if prev == nil {
		return true
	}
	return prev.Status != status || prev.Reason != reason || prev.Message != message || prev.ObservedGeneration != generation
}
