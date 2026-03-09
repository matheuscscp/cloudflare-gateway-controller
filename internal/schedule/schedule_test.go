// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

// Based on https://github.com/controlplaneio-fluxcd/flux-operator/blob/c225eff4f88a67b1e560ae3968553f2de6e6ced0/internal/schedule/scheduler_test.go

// Copyright 2025 Stefan Prodan.
// SPDX-License-Identifier: AGPL-3.0

package schedule_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/schedule"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("failed to parse time %q: %v", s, err)
	}
	return tt
}

func TestParse(t *testing.T) {
	now := mustParseTime(t, "2025-01-01T12:10:00Z")

	for _, tt := range []struct {
		spec     string
		timeZone string
		trigger  string
		err      string
	}{
		{
			spec:    "0 3 * * *",
			trigger: "2025-01-02T03:00:00Z",
		},
		{
			spec:     "0 5 * * *",
			timeZone: "America/Los_Angeles",
			trigger:  "2025-01-01T13:00:00Z",
		},
		{
			spec:     "0 5 * * *",
			timeZone: "UTC",
			trigger:  "2025-01-02T05:00:00Z",
		},
		{
			spec: "invalid-cron",
			err:  "failed to parse cron spec 'invalid-cron':",
		},
		{
			spec:     "invalid-cron",
			timeZone: "America/Los_Angeles",
			err:      "failed to parse cron spec 'invalid-cron' with timezone 'America/Los_Angeles':",
		},
	} {
		tz := strings.ReplaceAll(tt.timeZone, "/", "_")
		name := fmt.Sprintf("spec=%s,timeZone=%s", tt.spec, tz)
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)

			s, err := schedule.Parse(tt.spec, tt.timeZone)
			if tt.err != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.err))
				return
			}

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(s).NotTo(BeNil())

			trigger := s.Next(now)
			g.Expect(trigger).To(Equal(mustParseTime(t, tt.trigger)))
		})
	}
}

func TestGetPrevAndNextTriggers(t *testing.T) {
	for _, tt := range []struct {
		name     string
		cron     string
		timeZone string
		now      string
		prev     string
		next     string
	}{
		{
			name: "feb 29, now far apart from prev and next",
			cron: "0 0 29 2 *",
			now:  "2025-01-01T12:00:00Z",
			prev: "2024-02-29T00:00:00Z",
			next: "2028-02-29T00:00:00Z",
		},
		{
			name: "now equals prev",
			cron: "0 * * * *",
			now:  "2025-01-01T12:00:00Z",
			prev: "2025-01-01T12:00:00Z",
			next: "2025-01-01T13:00:00Z",
		},
		{
			name: "now right after prev",
			cron: "0 * * * *",
			now:  "2025-01-01T12:00:01Z",
			prev: "2025-01-01T12:00:00Z",
			next: "2025-01-01T13:00:00Z",
		},
		{
			name: "now right before next",
			cron: "0 * * * *",
			now:  "2025-01-01T11:59:59Z",
			prev: "2025-01-01T11:00:00Z",
			next: "2025-01-01T12:00:00Z",
		},
		{
			name:     "with timezone",
			cron:     "0 18 * * 4",
			timeZone: "America/Los_Angeles",
			now:      "2025-03-07T12:00:00Z",
			prev:     "2025-03-07T02:00:00Z",
			next:     "2025-03-14T01:00:00Z",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			s, err := schedule.Parse(tt.cron, tt.timeZone)
			g.Expect(err).NotTo(HaveOccurred())

			now := mustParseTime(t, tt.now)

			prev, next := schedule.GetPrevAndNextTriggers(s, now)

			g.Expect(prev).To(Equal(mustParseTime(t, tt.prev)))
			g.Expect(next).To(Equal(mustParseTime(t, tt.next)))
		})
	}
}
