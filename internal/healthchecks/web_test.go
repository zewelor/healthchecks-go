package healthchecks

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dashboardEngine(t *testing.T) *Engine {
	t.Helper()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	config := `public_url: http://localhost:8000
webhook:
  name: Incident desk
  url: https://example.invalid/hook
checks:
  - uuid: 11111111-1111-4111-8111-111111111111
    name: Heartbeat <img src=x onerror=alert(1)>
    tags: system, core
    every: 5m
    grace: 1m
    notify: true
    region: europe-west
    depends_on_regions: [us-east]
  - uuid: 22222222-2222-4222-8222-222222222222
    name: Queue worker
    every: 10m
    grace: 2m
`
	return NewEngine(parseTestConfig(t, config), func() time.Time { return now })
}

func dashboardRequest(e *Engine, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	renderDashboard(w, r, e)
	return w
}

func TestDashboardIndexUsesSafeKeysAndShowsOneCardPerCheck(t *testing.T) {
	e := dashboardEngine(t)
	e.Ping(testUUID, Input{Kind: "fail"})
	e.SetReloadError(`<invalid config>`)
	w := dashboardRequest(e, http.MethodGet, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if got := strings.Count(body, `class="check-card`); got != 2 {
		t.Fatalf("check cards = %d, want one per configured check", got)
	}
	if strings.Index(body, `data-status="down"`) < 0 || strings.Index(body, `data-status="down"`) > strings.Index(body, `data-status="awaiting"`) {
		t.Fatal("down check should appear before checks in other states")
	}
	if strings.Contains(body, testUUID) || strings.Contains(body, "22222222-2222-4222-8222-222222222222") {
		t.Fatal("dashboard exposed a configuration UUID")
	}
	if !strings.Contains(body, "/checks/"+e.Snapshot()[0].Key) {
		t.Fatal("dashboard detail links must use the unique check key")
	}
	if strings.Contains(body, `<img src=x`) || !strings.Contains(body, `&lt;img`) {
		t.Fatal("check name was not HTML escaped")
	}
	if !strings.Contains(strings.ToLower(body), "ping logs") || !strings.Contains(strings.ToLower(body), "reload") || strings.Contains(body, `<invalid config>`) || !strings.Contains(body, `&lt;invalid config&gt;`) {
		t.Fatalf("dashboard should explain process-local logs and surface escaped reload errors (logs=%t reload=%t escaped=%t raw=%t)", strings.Contains(body, "ping logs"), strings.Contains(body, "reload"), strings.Contains(body, `&lt;invalid config&gt;`), strings.Contains(body, `<invalid config>`))
	}
}

func TestDashboardDetailShowsScheduleDependenciesWebhookAndEscapedEvents(t *testing.T) {
	e := dashboardEngine(t)
	views := e.Snapshot()
	var check CheckView
	for _, view := range views {
		if view.Name == "Heartbeat <img src=x onerror=alert(1)>" {
			check = view
		}
	}
	if check.Key == "" {
		t.Fatal("heartbeat check not found")
	}
	e.Ping(testUUID, Input{Kind: "start", RID: "run-123"})
	e.Ping(testUUID, Input{Kind: "success", RID: "run-123"})
	exitCode := 2
	e.Ping(testUUID, Input{Kind: "log", Body: `<script>alert("log")</script>`, RID: "run-123", ExitCode: &exitCode, Truncated: true})
	e.mu.Lock()
	e.checks[testUUID].deliveryError = `temporary <failure>`
	e.mu.Unlock()
	w := dashboardRequest(e, http.MethodGet, "/checks/"+check.Key)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Every 5m", "europe-west", "us-east", "Incident desk", "run-123", "log", "heartbeat", "Exit code: 2", "Duration: 0s", "Log was truncated"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("detail page missing %q", want)
		}
	}
	if strings.Contains(body, `<script>alert`) || !strings.Contains(body, `&lt;script&gt;`) {
		t.Fatal("event log body was not HTML escaped")
	}
	if strings.Contains(body, `<img src=x`) || strings.Contains(body, `<failure>`) {
		t.Fatal("detail data was not HTML escaped")
	}
	if strings.Contains(body, testUUID) {
		t.Fatal("detail page exposed configuration UUID")
	}
}

func TestDashboardExactPathsAndMethods(t *testing.T) {
	e := dashboardEngine(t)
	key := e.Snapshot()[0].Key
	encodedFirstCharacter := "/checks/%" + hex.EncodeToString([]byte{key[0]}) + key[1:]
	for _, path := range []string{"/checks", "/checks/", "/checks/" + key + "/", "/checks/not-a-key", "/checks/" + key + "/extra", encodedFirstCharacter} {
		w := dashboardRequest(e, http.MethodGet, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %q status = %d, want 404", path, w.Code)
		}
	}
	w := dashboardRequest(e, http.MethodPost, "/")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST status/Allow = %d/%q, want 405/GET, HEAD", w.Code, w.Header().Get("Allow"))
	}
}

func TestDashboardHeadHasHeadersButNoBody(t *testing.T) {
	e := dashboardEngine(t)
	paths := []string{"/", "/checks/" + e.Snapshot()[0].Key}
	for _, path := range paths {
		w := dashboardRequest(e, http.MethodHead, path)
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Errorf("HEAD %q status/body = %d/%d, want 200/0", path, w.Code, w.Body.Len())
		}
		if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("HEAD %q Content-Type = %q", path, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("HEAD %q Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestDashboardRefreshControlUsesStaticHashedScript(t *testing.T) {
	e := dashboardEngine(t)
	key := e.Snapshot()[0].Key
	paths := []string{"/", "/checks/" + key}
	hash := sha256.Sum256([]byte(dashboardRefreshScript))
	policy := "script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'"
	for _, path := range paths {
		w := dashboardRequest(e, http.MethodGet, path)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %q status = %d, want 200", path, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<button class="refresh-toggle" id="refresh-toggle" type="button" aria-pressed="false">Pause auto-refresh</button>`) {
			t.Errorf("GET %q missing the pause control", path)
		}
		if !strings.Contains(body, "<script>"+dashboardRefreshScript+"</script>") || strings.Contains(body, `http-equiv="refresh"`) {
			t.Errorf("GET %q did not render the static controllable refresh script", path)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, policy) || strings.Contains(csp, "script-src 'unsafe-inline'") {
			t.Errorf("GET %q CSP does not hash the static script: %q", path, csp)
		}
		if strings.Contains(body, `<img src=x`) || strings.Contains(body, `<script>alert`) {
			t.Errorf("GET %q rendered user data as executable HTML", path)
		}
	}
}
