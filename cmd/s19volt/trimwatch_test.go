package main

import (
	"testing"
	"time"
)

// The trim watchdog's decision, on the 2026-11-26 sequences.
func TestTrimWatchReason(t *testing.T) {
	on := time.Date(2026, 11, 26, 21, 15, 51, 0, time.UTC) // trim came on
	old := on.Add(-time.Hour)
	cases := []struct {
		name            string
		stage           int
		retune, restart time.Time
		want            bool
	}{
		{"settled stage 4, nothing since the trim", 4, old, old, false},
		{"evening: profile failed, retuning after the trim came on", 2, on.Add(20 * time.Minute), old, true},
		{"retune line seen while still reading stage 4", 4, on.Add(time.Minute), old, true},
		{"Braiins back in preheat (stage 1)", 1, old, old, true},
		{"bosminer restarted under the trim", 0, old, on.Add(time.Minute), true},
		{"old retune and restart from before the trim: no action", 4, old, old, false},
	}
	for _, c := range cases {
		got := watchReason(c.stage, c.retune, c.restart, "S4 it14 event: profile failed, retuning", on)
		if (got != "") != c.want {
			t.Errorf("%s: reason %q, want trip=%v", c.name, got, c.want)
		}
	}
}
