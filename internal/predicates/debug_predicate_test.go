// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package predicates_test

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/predicates"
)

func TestDebug(t *testing.T) {
	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test",
			Namespace:       "default",
			ResourceVersion: "1",
			Generation:      1,
		},
	}

	t.Run("create matches when inner matches", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return true },
		})
		g.Expect(p.Create(event.CreateEvent{Object: obj})).To(BeTrue())
	})

	t.Run("create does not match when inner does not match", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return false },
		})
		g.Expect(p.Create(event.CreateEvent{Object: obj})).To(BeFalse())
	})

	t.Run("update matches when inner matches", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool { return true },
		})
		g.Expect(p.Update(event.UpdateEvent{ObjectOld: obj, ObjectNew: obj})).To(BeTrue())
	})

	t.Run("update does not match when inner does not match", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool { return false },
		})
		g.Expect(p.Update(event.UpdateEvent{ObjectOld: obj, ObjectNew: obj})).To(BeFalse())
	})

	t.Run("delete matches when inner matches", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			DeleteFunc: func(e event.DeleteEvent) bool { return true },
		})
		g.Expect(p.Delete(event.DeleteEvent{Object: obj})).To(BeTrue())
	})

	t.Run("delete does not match when inner does not match", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
		})
		g.Expect(p.Delete(event.DeleteEvent{Object: obj})).To(BeFalse())
	})

	t.Run("generic matches when inner matches", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			GenericFunc: func(e event.GenericEvent) bool { return true },
		})
		g.Expect(p.Generic(event.GenericEvent{Object: obj})).To(BeTrue())
	})

	t.Run("generic does not match when inner does not match", func(t *testing.T) {
		g := NewWithT(t)
		p := predicates.Debug("test-source", predicate.Funcs{
			GenericFunc: func(e event.GenericEvent) bool { return false },
		})
		g.Expect(p.Generic(event.GenericEvent{Object: obj})).To(BeFalse())
	})
}
