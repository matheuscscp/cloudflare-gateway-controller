// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package predicates

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ConditionStatusChanged returns a predicate that fires on create, delete, and
// generic events, but only fires on updates when the status of the named condition
// changes. getConditions extracts the condition slice from the object.
func ConditionStatusChanged(conditionType string, getConditions func(client.Object) []metav1.Condition) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldStatus := conditionStatus(getConditions(e.ObjectOld), conditionType)
			newStatus := conditionStatus(getConditions(e.ObjectNew), conditionType)
			return oldStatus != newStatus
		},
	}
}

func conditionStatus(conds []metav1.Condition, conditionType string) metav1.ConditionStatus {
	for _, c := range conds {
		if c.Type == conditionType {
			return c.Status
		}
	}
	return ""
}
