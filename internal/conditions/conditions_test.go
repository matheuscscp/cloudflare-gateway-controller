// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package conditions_test

import (
	"testing"
	"time"

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

func TestSet(t *testing.T) {
	oldTime := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newTime := metav1.NewTime(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))

	t.Run("preserves LastTransitionTime when status unchanged", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: oldTime},
		}
		desired := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: newTime, Reason: "OK"},
		}
		result := conditions.Set(existing, desired)
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].LastTransitionTime).To(Equal(oldTime))
		g.Expect(result[0].Reason).To(Equal("OK"))
	})

	t.Run("updates LastTransitionTime when status changed", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: oldTime},
		}
		desired := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, LastTransitionTime: newTime},
		}
		result := conditions.Set(existing, desired)
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].LastTransitionTime).To(Equal(newTime))
	})

	t.Run("new condition gets desired LastTransitionTime", func(t *testing.T) {
		g := NewWithT(t)
		result := conditions.Set(nil, []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: newTime},
		})
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].LastTransitionTime).To(Equal(newTime))
	})

	t.Run("replaces full list", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue},
			{Type: "Accepted", Status: metav1.ConditionTrue},
		}
		desired := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse},
		}
		result := conditions.Set(existing, desired)
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].Type).To(Equal("Ready"))
	})
}

func TestUpsert(t *testing.T) {
	oldTime := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newTime := metav1.NewTime(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))

	t.Run("updates existing preserving LastTransitionTime", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: oldTime, Reason: "old"},
		}
		result := conditions.Upsert(existing, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: newTime, Reason: "new",
		})
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].LastTransitionTime).To(Equal(oldTime))
		g.Expect(result[0].Reason).To(Equal("new"))
	})

	t.Run("updates existing with new LastTransitionTime on status change", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: oldTime},
		}
		result := conditions.Upsert(existing, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, LastTransitionTime: newTime,
		})
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].LastTransitionTime).To(Equal(newTime))
		g.Expect(result[0].Status).To(Equal(metav1.ConditionFalse))
	})

	t.Run("appends new condition", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Accepted", Status: metav1.ConditionTrue},
		}
		result := conditions.Upsert(existing, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: newTime,
		})
		g.Expect(result).To(HaveLen(2))
		g.Expect(result[1].Type).To(Equal("Ready"))
		g.Expect(result[1].LastTransitionTime).To(Equal(newTime))
	})

	t.Run("appends to nil slice", func(t *testing.T) {
		g := NewWithT(t)
		result := conditions.Upsert(nil, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue,
		})
		g.Expect(result).To(HaveLen(1))
	})

	t.Run("modifies slice in place when updating", func(t *testing.T) {
		g := NewWithT(t)
		existing := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "old"},
		}
		result := conditions.Upsert(existing, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "new",
		})
		// Returns the same slice when updating in place.
		g.Expect(&result[0]).To(BeIdenticalTo(&existing[0]))
	})
}

func TestRemove(t *testing.T) {
	t.Run("removes existing condition", func(t *testing.T) {
		g := NewWithT(t)
		conds := []metav1.Condition{
			{Type: "Ready"},
			{Type: "Accepted"},
		}
		result := conditions.Remove(conds, "Ready")
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].Type).To(Equal("Accepted"))
	})

	t.Run("returns unchanged slice when not found", func(t *testing.T) {
		g := NewWithT(t)
		conds := []metav1.Condition{
			{Type: "Ready"},
		}
		result := conditions.Remove(conds, "Missing")
		g.Expect(result).To(HaveLen(1))
		g.Expect(result[0].Type).To(Equal("Ready"))
	})

	t.Run("nil slice", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(conditions.Remove(nil, "Ready")).To(BeNil())
	})
}
