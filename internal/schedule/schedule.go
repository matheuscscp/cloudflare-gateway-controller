// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// Based on https://github.com/controlplaneio-fluxcd/flux-operator/blob/c225eff4f88a67b1e560ae3968553f2de6e6ced0/internal/schedule/scheduler.go

// Copyright 2025 Stefan Prodan.
// SPDX-License-Identifier: AGPL-3.0

package schedule

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	// cronLookBack is the maximum time range a previous cron trigger
	// could have occurred in the past. It's 5 years because a cron
	// schedule can be defined for Feb 29, which only occurs every 4
	// years (0 0 29 2 *).
	cronLookBack = 5 * 365 * 24 * time.Hour
)

// Parse parses a cron schedule specification and returns a cron.Schedule.
func Parse(spec, timeZone string) (cron.Schedule, error) {
	cronSpec := spec
	if timeZone != "" {
		cronSpec = fmt.Sprintf("CRON_TZ=%s %s", timeZone, spec)
	}
	s, err := cron.
		NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).
		Parse(cronSpec)
	if err == nil {
		return s, nil
	}
	if timeZone != "" {
		return nil, fmt.Errorf("failed to parse cron spec '%s' with timezone '%s': %w", spec, timeZone, err)
	}
	return nil, fmt.Errorf("failed to parse cron spec '%s': %w", spec, err)
}

// GetPrevAndNextTriggers returns the previous and next triggers for the given
// schedule spec and the current time.
func GetPrevAndNextTriggers(schedule cron.Schedule, now time.Time) (time.Time, time.Time) {
	next := schedule.Next(now)
	start := now.Add(-cronLookBack)
	end := next.Add(-1)
	prev := binarySearchPreviousTrigger(schedule, start, end, next)
	return prev, next
}

// binarySearchPreviousTrigger finds the previous trigger for the schedule
// by performing a binary search. If no previous trigger is found, zero is
// returned.
func binarySearchPreviousTrigger(schedule cron.Schedule, start, end, next time.Time) time.Time {
	if end.Before(start) {
		return time.Time{}
	}

	middle := start.Add(end.Sub(start) / 2)
	middleTrigger := schedule.Next(middle)

	if !middleTrigger.Before(next) {
		return binarySearchPreviousTrigger(schedule, start, middle.Add(-1), next)
	}

	closerTrigger := binarySearchPreviousTrigger(schedule, middleTrigger, end, next)
	if !closerTrigger.IsZero() {
		return closerTrigger
	}

	return middleTrigger
}
