package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"healthchecks-go/internal/healthchecks"
)

const uuid = "11111111-1111-4111-8111-111111111111"

func TestPingLogsMatchDashboard(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	config := "public_url: https://status.example.test\nchecks:\n  - uuid: " + uuid + "\n    name: Backup\n    every: 5m\n    grace: 1m\n"
	parsed, err := healthchecks.ParseConfig([]byte(config), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	e := healthchecks.NewEngine(parsed, time.Now)
	mode, err := healthchecks.ParsePingLogMode("full")
	if err != nil {
		t.Fatal(err)
	}
	e.SetPingLogMode(mode)
	server := httptest.NewServer(healthchecks.NewHandler(e, ""))
	defer server.Close()
	const rid = "22222222-2222-4222-8222-222222222222"
	for _, ping := range []struct{ path, body string }{
		{"/start?rid=" + rid, "starting"},
		{"/7?rid=" + rid, "failure\n" + strings.Repeat("x", healthchecks.BodyLimit)},
	} {
		response, err := server.Client().Post(server.URL+"/ping/"+uuid+ping.path, "text/plain", strings.NewReader(ping.body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("ping returned %d", response.StatusCode)
		}
	}
	view := e.Snapshot()[0]
	response, err := server.Client().Get(server.URL + "/checks/" + view.Key)
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Contains(page, []byte("failure\n")) || !bytes.Contains(page, []byte("Log was truncated")) {
		t.Fatalf("dashboard did not show truncated log: status=%d error=%v", response.StatusCode, err)
	}
	lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected two JSON log records, got %d", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal(lines[1], &entry); err != nil {
		t.Fatal(err)
	}
	event := view.Events[0]
	if entry["body"] != event.Body || entry["event_id"] != event.ID || entry["rid"] != rid || entry["truncated"] != true || entry["kind"] != "fail" || entry["exit_code"] != float64(7) {
		t.Fatalf("JSON record differs from dashboard event: %+v", entry)
	}
	if bytes.Contains(logs.Bytes(), []byte(uuid)) {
		t.Fatal("ping URL credential leaked into logs")
	}
}

func TestExplicitIdentityAcrossReloads(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	config := "public_url: https://status.example.test\nchecks:\n  - uuid: " + uuid + "\n    name: Heartbeat\n    every: 5m\n    grace: 1m\n"
	parse := func(text string) *healthchecks.Config {
		t.Helper()
		c, err := healthchecks.ParseConfig([]byte(text), lookup)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	initial := parse(config)
	id := initial.Checks[0].UUID
	e := healthchecks.NewEngine(initial, time.Now)
	server := httptest.NewServer(healthchecks.NewHandler(e, ""))
	defer server.Close()
	ping := func(id, action string) int {
		t.Helper()
		response, err := server.Client().Post(server.URL+"/ping/"+id+action, "text/plain", strings.NewReader("retained log"))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	replace := func(text string) {
		t.Helper()
		if err := os.WriteFile(path+".next", []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".next", path); err != nil {
			t.Fatal(err)
		}
	}
	replace(config)
	w := healthchecks.NewConfigWatcher(path, e, lookup)
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if ping(id, "/fail") != 200 {
		t.Fatal("configured UUID ping rejected")
	}
	config = strings.Replace(config, "every: 5m", "every: 10m\n    depends_on_regions: [region-a]", 1)
	replace(config)
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Status != "down" || len(got.Events) != 1 || len(got.DependsOnRegions) != 1 || got.DependsOnRegions[0] != "region-a" {
		t.Fatalf("metadata reload lost check state: %+v", got)
	}
	replace(config + strings.Split(config, "checks:\n")[1])
	if _, err := w.Check(); err == nil || !strings.Contains(err.Error(), "duplicate UUID") {
		t.Fatalf("duplicate UUID reload error: %v", err)
	}
	if got := e.Snapshot()[0]; got.Status != "down" || len(got.Events) != 1 {
		t.Fatal("invalid duplicate-UUID reload destroyed active state")
	}
	// Missing UUIDs must reject a reload without losing active state.
	replace(strings.Replace(config, "  - uuid: "+id+"\n    name:", "  - name:", 1))
	if _, err := w.Check(); err == nil || !strings.Contains(err.Error(), "uuid") {
		t.Fatalf("missing UUID reload error: %v", err)
	}
	if got := e.Snapshot()[0]; got.Status != "down" || len(got.Events) != 1 || ping(id, "/log") != 200 {
		t.Fatal("missing UUID reload destroyed active state")
	}
	// Renaming a check keeps its explicit identity and state.
	renamed := strings.Replace(config, "Heartbeat", "Renamed", 1)
	replace(renamed)
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Name != "Renamed" || got.Status != "down" || len(got.Events) != 2 || e.Info().ReloadError != "" {
		t.Fatal("explicit UUID did not preserve identity during rename")
	}
	// Changing the UUID deliberately starts a new check.
	const newID = "22222222-2222-4222-8222-222222222222"
	replaced := strings.Replace(renamed, id, newID, 1)
	replace(replaced)
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if ping(id, "") != 404 {
		t.Fatal("replaced check retained its old ping identity")
	}
	if got := e.Snapshot()[0]; got.Status != "awaiting" || len(got.Events) != 0 {
		t.Fatal("replaced check retained old state")
	}
	if ping(newID, "") != 200 || e.Snapshot()[0].Status != "up" {
		t.Fatal("replacement check does not receive pings")
	}
	if restarted := healthchecks.NewEngine(parse(replaced), time.Now); restarted.Snapshot()[0].Key != e.Snapshot()[0].Key {
		t.Fatal("restart changed explicit identity")
	}
}

func TestServiceLifecycle(t *testing.T) {
	var ticks atomic.Int64
	ticks.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	clock := func() time.Time { return time.Unix(0, ticks.Load()).UTC() }
	events := make(chan healthchecks.WebhookEvent, 10)
	var attempts atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event healthchecks.WebhookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			return
		}
		events <- event
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	config := fmt.Sprintf(`public_url: https://status.example.test
webhook:
  name: fixture
  url: %q
checks:
  - uuid: %s
    name: Backup
    description: "Nightly <database> snapshot & verification"
    every: 5m
    grace: 1m
    depends_on_regions: [region-b, region-a]
    notify: true
`, receiver.URL, uuid)
	parsed, err := healthchecks.ParseConfig([]byte(config), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	e := healthchecks.NewEngine(parsed, clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.RunWebhook(ctx) }()
	defer func() { cancel(); <-done }()
	server := httptest.NewServer(healthchecks.NewHandler(e, "test-token"))
	defer server.Close()
	call := func(method, path, body, token string) (int, string) {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	status, _ := call("GET", "/metrics", "", "")
	if status != 401 {
		t.Fatal("metrics authentication missing")
	}
	if status, body := call("GET", "/api/v3/status/", "", ""); status != 200 || body != "OK" {
		t.Fatal("readiness failed")
	}
	if status, body := call("HEAD", "/ping/"+uuid+"/start?rid="+uuid, "", ""); status != 200 || body != "" {
		t.Fatal("HEAD start failed")
	}
	call("POST", "/ping/"+uuid+"/log", strings.Repeat("log ", 5000), "")
	ticks.Add(int64(time.Minute))
	e.Evaluate()
	select {
	case event := <-events:
		if event.Status != "down" || event.UniqueKey == "" || strings.Contains(event.URL, uuid) {
			t.Fatalf("down event: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no down notification after run timeout/retry")
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts: %d", attempts.Load())
	}
	status, metrics := call("GET", "/metrics", "", "test-token")
	if status != 200 || !strings.Contains(metrics, `} 0`) || !strings.Contains(metrics, `depends_on_regions="region-a,region-b"`) || strings.Contains(metrics, `,region="`) || strings.Contains(metrics, `tags=`) || strings.Contains(metrics, uuid) {
		t.Fatal(metrics)
	}
	view := e.Snapshot()[0]
	status, detail := call("GET", "/checks/"+view.Key, "", "")
	if status != 200 || !strings.Contains(detail, "truncated") {
		t.Fatal("missing bounded log detail")
	}
	if !strings.Contains(detail, "Nightly &lt;database&gt; snapshot &amp; verification") {
		t.Fatal("missing escaped check description")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	updated := strings.Replace(config, "name: Backup", "name: Renamed backup", 1)
	updated = strings.Replace(updated, "Nightly <database> snapshot & verification", "Updated backup instructions", 1)
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	watcher := healthchecks.NewConfigWatcher(path, e, os.LookupEnv)
	if _, err := watcher.Check(); err != nil {
		t.Fatal(err)
	}
	if v := e.Snapshot()[0]; v.Status != "down" || len(v.Events) != 2 {
		t.Fatal("reload lost failure/history")
	}
	if err := os.WriteFile(path, []byte("checks: invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := watcher.Check(); err == nil {
		t.Fatal("invalid reload accepted")
	}
	_, index := call("GET", "/", "", "")
	if !strings.Contains(index, "reload") || !strings.Contains(index, "Renamed backup") || !strings.Contains(index, "Updated backup instructions") {
		t.Fatal("reload error/active definition not rendered")
	}
	call("POST", "/ping/"+uuid, "<script>alert('test')</script>", "")
	select {
	case event := <-events:
		if event.Status != "up" {
			t.Fatalf("recovery: %s", event.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no recovery")
	}
	_, detail = call("GET", "/checks/"+view.Key, "", "")
	if strings.Contains(detail, "<script>alert") || !strings.Contains(detail, "&lt;script&gt;") {
		t.Fatal("unsafe log rendering")
	}
	if status, _ := call("GET", "/ping/22222222-2222-4222-8222-222222222222?create=1", "", ""); status != 404 {
		t.Fatal("created an unknown check")
	}
	restarted := healthchecks.NewEngine(parsed, clock)
	if v := restarted.Snapshot()[0]; v.Status != "awaiting" || len(v.Events) != 0 || !v.LastSuccess.IsZero() {
		t.Fatal("restart retained or fabricated state")
	}
}
