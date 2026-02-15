// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// debugPredicate wraps a predicate and logs when it matches, to help identify
// which watch event triggers a reconciliation.
func debugPredicate(source string, inner predicate.Predicate) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			if inner.Create(e) {
				log.Log.V(1).Info("Watch event matched",
					"source", source, "event", "create",
					"name", e.Object.GetName(), "namespace", e.Object.GetNamespace())
				return true
			}
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if inner.Update(e) {
				log.Log.V(1).Info("Watch event matched",
					"source", source, "event", "update",
					"name", e.ObjectNew.GetName(), "namespace", e.ObjectNew.GetNamespace(),
					"oldRV", e.ObjectOld.GetResourceVersion(), "newRV", e.ObjectNew.GetResourceVersion(),
					"oldGen", e.ObjectOld.GetGeneration(), "newGen", e.ObjectNew.GetGeneration())
				return true
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			if inner.Delete(e) {
				log.Log.V(1).Info("Watch event matched",
					"source", source, "event", "delete",
					"name", e.Object.GetName(), "namespace", e.Object.GetNamespace())
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			if inner.Generic(e) {
				log.Log.V(1).Info("Watch event matched",
					"source", source, "event", "generic",
					"name", e.Object.GetName(), "namespace", e.Object.GetNamespace())
				return true
			}
			return false
		},
	}
}
