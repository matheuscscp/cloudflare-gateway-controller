// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package conditions_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

func TestFind(t *testing.T) {
	conds := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"},
		{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NotAccepted"},
	}

	t.Run("found", func(t *testing.T) {
		c := conditions.Find(conds, "Ready")
		if c == nil {
			t.Fatal("Find(Ready) = nil, want non-nil")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("Find(Ready).Status = %v, want True", c.Status)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if c := conditions.Find(conds, "Missing"); c != nil {
			t.Errorf("Find(Missing) = %v, want nil", c)
		}
	})

	t.Run("nil slice", func(t *testing.T) {
		if c := conditions.Find(nil, "Ready"); c != nil {
			t.Errorf("Find(nil, Ready) = %v, want nil", c)
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		if c := conditions.Find([]metav1.Condition{}, "Ready"); c != nil {
			t.Errorf("Find(empty, Ready) = %v, want nil", c)
		}
	})

	t.Run("returns pointer into slice", func(t *testing.T) {
		slice := []metav1.Condition{
			{Type: "Ready", Reason: "original"},
		}
		c := conditions.Find(slice, "Ready")
		c.Reason = "modified"
		if slice[0].Reason != "modified" {
			t.Error("Find should return pointer into the original slice")
		}
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
		if conditions.Changed(existing, "Ready", metav1.ConditionTrue, "OK", "all good", 1) {
			t.Error("Changed() = true for identical condition")
		}
	})

	t.Run("condition not found", func(t *testing.T) {
		if !conditions.Changed(existing, "Missing", metav1.ConditionTrue, "OK", "all good", 1) {
			t.Error("Changed() = false for missing condition")
		}
	})

	t.Run("status changed", func(t *testing.T) {
		if !conditions.Changed(existing, "Ready", metav1.ConditionFalse, "OK", "all good", 1) {
			t.Error("Changed() = false when status changed")
		}
	})

	t.Run("reason changed", func(t *testing.T) {
		if !conditions.Changed(existing, "Ready", metav1.ConditionTrue, "Different", "all good", 1) {
			t.Error("Changed() = false when reason changed")
		}
	})

	t.Run("message changed", func(t *testing.T) {
		if !conditions.Changed(existing, "Ready", metav1.ConditionTrue, "OK", "different", 1) {
			t.Error("Changed() = false when message changed")
		}
	})

	t.Run("generation changed", func(t *testing.T) {
		if !conditions.Changed(existing, "Ready", metav1.ConditionTrue, "OK", "all good", 2) {
			t.Error("Changed() = false when generation changed")
		}
	})

	t.Run("nil slice", func(t *testing.T) {
		if !conditions.Changed(nil, "Ready", metav1.ConditionTrue, "OK", "msg", 1) {
			t.Error("Changed() = false for nil slice")
		}
	})
}

func TestApplyChanged(t *testing.T) {
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
		cond := &acmetav1.ConditionApplyConfiguration{
			Type:    new("Ready"),
			Status:  new(metav1.ConditionStatus(metav1.ConditionTrue)),
			Reason:  new("OK"),
			Message: new("all good"),
		}
		if conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = true for identical condition")
		}
	})

	t.Run("changed", func(t *testing.T) {
		cond := &acmetav1.ConditionApplyConfiguration{
			Type:    new("Ready"),
			Status:  new(metav1.ConditionStatus(metav1.ConditionFalse)),
			Reason:  new("Failed"),
			Message: new("something broke"),
		}
		if !conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = false for changed condition")
		}
	})

	t.Run("nil type", func(t *testing.T) {
		cond := &acmetav1.ConditionApplyConfiguration{
			Status:  new(metav1.ConditionStatus(metav1.ConditionTrue)),
			Reason:  new("OK"),
			Message: new("all good"),
		}
		if !conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = false when Type is nil")
		}
	})

	t.Run("nil status", func(t *testing.T) {
		cond := &acmetav1.ConditionApplyConfiguration{
			Type:    new("Ready"),
			Reason:  new("OK"),
			Message: new("all good"),
		}
		if !conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = false when Status is nil")
		}
	})

	t.Run("nil reason", func(t *testing.T) {
		cond := &acmetav1.ConditionApplyConfiguration{
			Type:    new("Ready"),
			Status:  new(metav1.ConditionStatus(metav1.ConditionTrue)),
			Message: new("all good"),
		}
		if !conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = false when Reason is nil")
		}
	})

	t.Run("nil message", func(t *testing.T) {
		cond := &acmetav1.ConditionApplyConfiguration{
			Type:   new("Ready"),
			Status: new(metav1.ConditionStatus(metav1.ConditionTrue)),
			Reason: new("OK"),
		}
		if !conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = false when Message is nil")
		}
	})

	t.Run("condition not found in existing", func(t *testing.T) {
		cond := &acmetav1.ConditionApplyConfiguration{
			Type:    new("Missing"),
			Status:  new(metav1.ConditionStatus(metav1.ConditionTrue)),
			Reason:  new("OK"),
			Message: new("msg"),
		}
		if !conditions.ApplyChanged(existing, cond, 1) {
			t.Error("ApplyChanged() = false for missing condition")
		}
	})
}
