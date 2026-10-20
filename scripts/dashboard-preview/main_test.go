package main

import (
	"testing"
	"time"
)

func TestPreviewEngineAdvancesWithWallClock(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	base := now
	engine, err := newPreviewEngine(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		elapsed time.Duration
		status  string
	}{
		{0, "up"},
		{11 * time.Minute, "grace"},
		{31 * time.Minute, "down"},
	} {
		now = base.Add(step.elapsed)
		found := false
		for _, check := range engine.Snapshot() {
			if check.Name != "API heartbeat" {
				continue
			}
			found = true
			if check.Status != step.status {
				t.Errorf("after %s: status = %q, want %q", step.elapsed, check.Status, step.status)
			}
			if !check.NextDue.Equal(base.Add(10 * time.Minute)) {
				t.Errorf("preview should retain the seeded due time: %s", check.NextDue)
			}
		}
		if !found {
			t.Fatal("seeded API heartbeat missing")
		}
	}
}
