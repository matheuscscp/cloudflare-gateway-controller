// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package conditions_test

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

func TestFind(t *testing.T) {
	conds := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"},
		{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NotAccepted"},
	}

	t.Run("found", func(t *testing.T) {
		g := NewWithT(t)
		c := conditions.Find(conds, "Ready")
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
	})

	t.Run("not found", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Find(conds, "Missing")).To(BeNil())
	})

	t.Run("nil slice", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Find(nil, "Ready")).To(BeNil())
	})

	t.Run("empty slice", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Find([]metav1.Condition{}, "Ready")).To(BeNil())
	})

	t.Run("returns pointer into slice", func(t *testing.T) {
		g := NewWithT(t)
		slice := []metav1.Condition{
			{Type: "Ready", Reason: "original"},
		}
		c := conditions.Find(slice, "Ready")
		c.Reason = "modified"
		g.Expect(slice[0].Reason).To(Equal("modified"))
	})
}

func TestChanged(t *testing.T) {
	existing := []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "OK",
			Message:            "all good",
			ObservedGeneration: 1,
		},
	}

	t.Run("no change", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(existing, "Ready", metav1.ConditionTrue, "OK", "all good", 1)).To(BeFalse())
	})

	t.Run("condition not found", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(existing, "Missing", metav1.ConditionTrue, "OK", "all good", 1)).To(BeTrue())
	})

	t.Run("status changed", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(existing, "Ready", metav1.ConditionFalse, "OK", "all good", 1)).To(BeTrue())
	})

	t.Run("reason changed", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(existing, "Ready", metav1.ConditionTrue, "Different", "all good", 1)).To(BeTrue())
	})

	t.Run("message changed", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(existing, "Ready", metav1.ConditionTrue, "OK", "different", 1)).To(BeTrue())
	})

	t.Run("generation changed", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(existing, "Ready", metav1.ConditionTrue, "OK", "all good", 2)).To(BeTrue())
	})

	t.Run("nil slice", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Changed(nil, "Ready", metav1.ConditionTrue, "OK", "msg", 1)).To(BeTrue())
	})
}
