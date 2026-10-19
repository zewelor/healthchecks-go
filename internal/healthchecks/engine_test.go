package healthchecks

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newTestEngine(t *testing.T) (*Engine, *time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := NewEngine(parseTestConfig(t, simpleConfig), func() time.Time { return now })
	return e, &now
}

func onlyCheck(t *testing.T, e *Engine) CheckView {
	t.Helper()
	v := e.Snapshot()
	if len(v) != 1 {
		t.Fatalf("got %d checks", len(v))
	}
	return v[0]
}

func TestDeadlinesStartAndLogDoNotHideFailure(t *testing.T) {
	e, now := newTestEngine(t)
	if v := onlyCheck(t, e); v.Status != "awaiting" || !v.LastSuccess.IsZero() {
		t.Fatalf("initial state: %+v", v)
	}
	if found, err := e.Ping(testUUID, Input{Kind: "success"}); !found || err != nil {
		t.Fatal("unknown check")
	}
	*now = now.Add(4 * time.Minute)
	e.Ping(testUUID, Input{Kind: "start"})
	e.Ping(testUUID, Input{Kind: "log", Body: "still running"})
	*now = now.Add(time.Minute)
	if v := onlyCheck(t, e); v.Status != "down" || v.LastSuccess.Equal(*now) {
		t.Fatalf("run timeout: %+v", v)
	}
	e.Ping(testUUID, Input{Kind: "start"})
	if onlyCheck(t, e).Status != "down" {
		t.Fatal("start cleared an existing failure")
	}
	*now = now.Add(time.Minute)
	e.Ping(testUUID, Input{Kind: "log"})
	if onlyCheck(t, e).Status != "down" {
		t.Fatal("log refreshed success deadline")
	}
	e.Ping(testUUID, Input{Kind: "success"})
	if v := onlyCheck(t, e); v.Status != "up" || !v.Started.IsZero() {
		t.Fatalf("recovery: %+v", v)
	}
}

func TestGraceAndRestart(t *testing.T) {
	e, now := newTestEngine(t)
	*now = now.Add(5 * time.Minute)
	if onlyCheck(t, e).Status != "grace" {
		t.Fatal("expected grace")
	}
	*now = now.Add(time.Minute)
	if onlyCheck(t, e).Status != "down" {
		t.Fatal("deadline must include exact boundary")
	}
	restarted := NewEngine(parseTestConfig(t, simpleConfig), func() time.Time { return *now })
	if v := onlyCheck(t, restarted); v.Status != "awaiting" || !v.LastSuccess.IsZero() {
		t.Fatalf("restart: %+v", v)
	}
}

func TestCronFallBackNextDeadlineIsFuture(t *testing.T) {
	// The first 01:30 has passed, but the repeated hour has not reached 01:30.
	now := time.Date(2026, time.October, 25, 1, 20, 0, 0, time.UTC)
	config := strings.Replace(simpleConfig, "every: 5m", "cron: '30 1 * * *'\n    timezone: Europe/London", 1)
	e := NewEngine(parseTestConfig(t, config), func() time.Time { return now })
	e.Ping(testUUID, Input{Kind: "success"})
	view := onlyCheck(t, e)
	want := time.Date(2026, time.October, 26, 1, 30, 0, 0, time.UTC)
	if view.Status != "up" || !view.NextDue.Equal(want) {
		t.Fatalf("successful ping must wait for the next future occurrence: status=%s next=%s", view.Status, view.NextDue)
	}
}

func TestCronFallBackDoesNotExpectSecondPing(t *testing.T) {
	now := time.Date(2026, time.October, 25, 0, 30, 5, 0, time.UTC)
	config := strings.Replace(simpleConfig, "every: 5m", "cron: '30 1 * * *'\n    timezone: Europe/London", 1)
	e := NewEngine(parseTestConfig(t, config), func() time.Time { return now })
	e.Ping(testUUID, Input{Kind: "success"})
	want := time.Date(2026, time.October, 26, 1, 30, 0, 0, time.UTC)
	for _, tc := range []struct{ name, at, status string }{
		{"no-second-ping", "2026-10-25T01:32:00Z", "up"},
		{"before-tomorrow", "2026-10-26T01:29:59Z", "up"},
		{"tomorrow-grace", "2026-10-26T01:30:00Z", "grace"},
		{"tomorrow-missing-ping", "2026-10-26T01:31:00Z", "down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			now, err = time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			view := onlyCheck(t, e)
			if view.Status != tc.status || !view.NextDue.Equal(want) {
				t.Fatalf("at %s: status=%s, want %s; next=%s, want %s", now, view.Status, tc.status, view.NextDue, want)
			}
		})
	}
}

func TestRunIDs(t *testing.T) {
	for _, tc := range []struct {
		name, kind, rid string
		closed          bool
	}{
		{"matching success", "success", testUUID, true},
		{"unmatched success", "success", otherUUID, false},
		{"legacy success", "success", "", true},
		{"unmatched fail", "fail", otherUUID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, now := newTestEngine(t)
			e.Ping(testUUID, Input{Kind: "start", RID: testUUID})
			*now = now.Add(20 * time.Second)
			e.Ping(testUUID, Input{Kind: tc.kind, RID: tc.rid})
			v := onlyCheck(t, e)
			if v.Started.IsZero() != tc.closed {
				t.Fatalf("started=%v", v.Started)
			}
			if tc.rid == testUUID && (v.Duration == nil || *v.Duration != 20*time.Second) {
				t.Fatal("missing matched duration")
			}
		})
	}
}

func TestHistoryIsBoundedAndSeparateFromState(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Ping(testUUID, Input{Kind: "fail"})
	for i := 0; i < HistoryLimit+5; i++ {
		e.Ping(testUUID, Input{Kind: "log", Body: fmt.Sprint(i) + strings.Repeat("x", BodyLimit)})
	}
	v := onlyCheck(t, e)
	if v.Status != "down" || len(v.Events) != HistoryLimit || len(v.Events[0].Body) > BodyLimit || !v.Events[0].Truncated {
		t.Fatalf("bounded history/state: %+v", v)
	}
}

func TestReloadRetainsStateAndUsesOriginalTiming(t *testing.T) {
	e, now := newTestEngine(t)
	e.Ping(testUUID, Input{Kind: "success", Body: "retained"})
	*now = now.Add(2 * time.Minute)
	c := parseTestConfig(t, strings.ReplaceAll(strings.ReplaceAll(simpleConfig, "name: Heartbeat", "name: Renamed"), "every: 5m", "every: 3m"))
	e.Reload(c)
	v := onlyCheck(t, e)
	if v.Name != "Renamed" || len(v.Events) != 1 || !v.NextDue.Equal(now.Add(time.Minute)) {
		t.Fatalf("reload: %+v", v)
	}
	c.Checks[0].Paused = true
	e.Reload(c)
	e.Ping(testUUID, Input{Kind: "fail"})
	if onlyCheck(t, e).Status != "paused" {
		t.Fatal("ping resumed declared pause")
	}
	c = parseTestConfig(t, simpleConfig)
	e.Reload(c)
	if v := onlyCheck(t, e); v.Status != "awaiting" || !v.LastSuccess.IsZero() {
		t.Fatalf("unpause: %+v", v)
	}
}
