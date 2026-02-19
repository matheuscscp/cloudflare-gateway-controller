// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package predicates_test

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/predicates"
)

func TestConditionStatusChanged(t *testing.T) {
	condType := "Ready"
	getConditions := func(obj client.Object) []metav1.Condition {
		cm := obj.(*corev1.ConfigMap)
		if cm.Annotations == nil {
			return nil
		}
		// Simulate conditions via annotations for testing
		status, ok := cm.Annotations["readyStatus"]
		if !ok {
			return nil
		}
		return []metav1.Condition{
			{Type: condType, Status: metav1.ConditionStatus(status)},
		}
	}

	p := predicates.ConditionStatusChanged(condType, getConditions)

	t.Run("create always matches", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Create(event.CreateEvent{
			Object: &corev1.ConfigMap{},
		})).To(BeTrue())
	})

	t.Run("delete always matches", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Delete(event.DeleteEvent{
			Object: &corev1.ConfigMap{},
		})).To(BeTrue())
	})

	t.Run("generic always matches", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Generic(event.GenericEvent{
			Object: &corev1.ConfigMap{},
		})).To(BeTrue())
	})

	t.Run("update matches when condition status changes", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Update(event.UpdateEvent{
			ObjectOld: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"readyStatus": "False"},
				},
			},
			ObjectNew: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"readyStatus": "True"},
				},
			},
		})).To(BeTrue())
	})

	t.Run("update does not match when condition status is unchanged", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Update(event.UpdateEvent{
			ObjectOld: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"readyStatus": "True"},
				},
			},
			ObjectNew: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"readyStatus": "True"},
				},
			},
		})).To(BeFalse())
	})

	t.Run("update matches when condition appears", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Update(event.UpdateEvent{
			ObjectOld: &corev1.ConfigMap{},
			ObjectNew: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"readyStatus": "True"},
				},
			},
		})).To(BeTrue())
	})

	t.Run("update matches when condition disappears", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Update(event.UpdateEvent{
			ObjectOld: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"readyStatus": "True"},
				},
			},
			ObjectNew: &corev1.ConfigMap{},
		})).To(BeTrue())
	})

	t.Run("update does not match when both have no condition", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(p.Update(event.UpdateEvent{
			ObjectOld: &corev1.ConfigMap{},
			ObjectNew: &corev1.ConfigMap{},
		})).To(BeFalse())
	})
}
