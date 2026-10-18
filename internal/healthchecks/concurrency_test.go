package healthchecks

import (
	"strings"
	"sync"
	"testing"
)

func TestConcurrentReloadPingsAndSnapshots(t *testing.T) {
	e, _ := newTestEngine(t)
	a := parseTestConfig(t, simpleConfig)
	b := parseTestConfig(t, strings.Replace(simpleConfig, "Heartbeat", "Renamed", 1))
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			e.Ping(testUUID, Input{Kind: "start"})
			e.Ping(testUUID, Input{Kind: "success"})
		}
	})
	wg.Go(func() {
		for range 200 {
			e.Reload(a)
			e.Reload(b)
		}
	})
	wg.Go(func() {
		for range 200 {
			v := e.Snapshot()
			if len(v) != 1 || (v[0].Name != "Heartbeat" && v[0].Name != "Renamed") || len(v[0].Events) > HistoryLimit {
				t.Error("inconsistent snapshot")
			}
		}
	})
	wg.Wait()
	if len(e.Snapshot()[0].Events) != HistoryLimit {
		t.Fatal("reload discarded history")
	}
}

func TestUnpauseClearsCurrentFailureReason(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Ping(testUUID, Input{Kind: "fail"})
	c := parseTestConfig(t, simpleConfig)
	c.Checks[0].Paused = true
	e.Reload(c)
	c = parseTestConfig(t, simpleConfig)
	e.Reload(c)
	if v := onlyCheck(t, e); v.Reason != "" {
		t.Fatalf("stale failure reason after unpause: %s", v.Reason)
	}
}
