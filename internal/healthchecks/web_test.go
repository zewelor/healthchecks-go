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
    every: 5m
    grace: 1m
    notify: true
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
	if !strings.Contains(strings.ToLower(body), "reload") || strings.Contains(body, `<invalid config>`) || !strings.Contains(body, `&lt;invalid config&gt;`) {
		t.Fatal("dashboard should surface escaped reload errors")
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
	for _, want := range []string{"Every 5 minutes", "us-east", "Incident desk", "run-123", "log", "heartbeat", "Exit code: 2", "Duration: 0s", "Log was truncated"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("detail page missing %q", want)
		}
	}
	if strings.Contains(body, "<strong>Region</strong>") {
		t.Fatal("detail page shows removed region field")
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

func TestDashboardUsesStaticHashedScripts(t *testing.T) {
	e := dashboardEngine(t)
	key := e.Snapshot()[0].Key
	paths := []string{"/", "/checks/" + key}
	hash := sha256.Sum256([]byte(dashboardRefreshScript))
	policy := "script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'"
	themeHash := sha256.Sum256([]byte(dashboardThemeScript))
	themePolicy := "'sha256-" + base64.StdEncoding.EncodeToString(themeHash[:]) + "'"
	for _, path := range paths {
		w := dashboardRequest(e, http.MethodGet, path)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %q status = %d, want 200", path, w.Code)
		}
		body := w.Body.String()

		if !strings.Contains(body, "<script>"+dashboardRefreshScript+"</script>") || strings.Contains(body, `http-equiv="refresh"`) {
			t.Errorf("GET %q did not render the static controllable refresh script", path)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, policy) || !strings.Contains(csp, themePolicy) || strings.Contains(csp, "script-src 'unsafe-inline'") {
			t.Errorf("GET %q CSP does not hash the static script: %q", path, csp)
		}
		if strings.Contains(body, `<img src=x`) || strings.Contains(body, `<script>alert`) {
			t.Errorf("GET %q rendered user data as executable HTML", path)
		}
	}
}

func TestDashboardRelativeTime(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		delta time.Duration
		want  string
	}{
		{0, "now"}, {5 * time.Minute, "in 5 minutes"}, {-time.Minute, "1 minute ago"},
		{2 * time.Hour, "in 2 hours"}, {-24 * time.Hour, "1 day ago"}, {30 * time.Second, "in 30 seconds"},
		{24*time.Hour - 5*time.Second, "in 1 day"}, {time.Hour - 5*time.Second, "in 1 hour"}, {time.Minute - time.Second, "in 59 seconds"},
	} {
		if got := formatDashboardRelativeTime(now.Add(tc.delta), now); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.delta, got, tc.want)
		}
	}
	if got := formatDashboardRelativeTime(time.Time{}, now); got != "—" {
		t.Fatalf("unset time = %q", got)
	}
}

func TestDashboardCompactCheckTimes(t *testing.T) {
	e := dashboardEngine(t)
	body := dashboardRequest(e, http.MethodGet, "/").Body.String()
	if !strings.Contains(body, `datetime="2026-10-18T12:05:00Z"`) || !strings.Contains(body, `>in 5 minutes</time>`) {
		t.Fatal("next due should show a relative time with a machine-readable exact date")
	}
	if !strings.Contains(body, `data-full="Sun, 18 Oct 2026 12:05:00 UTC"`) {
		t.Fatal("full time should be available on hover and focus")
	}
	if strings.Contains(body, `<dt>Running</dt>`) || strings.Contains(body, `<dt>Schedule</dt>`) {
		t.Fatal("inactive running state and schedule should not clutter the overview")
	}
	e.Ping(testUUID, Input{Kind: "start"})
	body = dashboardRequest(e, http.MethodGet, "/").Body.String()
	if !strings.Contains(body, `Running since`) {
		t.Fatal("active runs should remain visible")
	}
}

func TestDashboardReadableSchedule(t *testing.T) {
	for _, tc := range []struct{ every, want string }{
		{"336h", "Every 14 days"}, {"24h", "Every 1 day"}, {"90m", "Every 1 hour 30 minutes"}, {"5m", "Every 5 minutes"}, {"1500ms", "Every 1.5 seconds"},
	} {
		e := NewEngine(parseTestConfig(t, strings.Replace(simpleConfig, "every: 5m", "every: "+tc.every, 1)), time.Now)
		body := dashboardRequest(e, http.MethodGet, "/checks/"+e.Snapshot()[0].Key).Body.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s: detail should show %q", tc.every, tc.want)
		}
	}
}

func TestDashboardSiteNameIsEscapedInHeadingAndTitles(t *testing.T) {
	e := dashboardEngine(t)
	e.mu.Lock()
	e.applied.SiteName = `Status <img src=x onerror=alert(1)>`
	e.mu.Unlock()
	for _, path := range []string{"/", "/checks/" + e.Snapshot()[0].Key} {
		body := dashboardRequest(e, http.MethodGet, path).Body.String()
		if !strings.Contains(body, "Status &lt;img") || strings.Contains(body, "Status <img") {
			t.Fatalf("%s: site name missing or unescaped", path)
		}
	}
}

func TestDashboardDetailUsesRelativeTimes(t *testing.T) {
	e := dashboardEngine(t)
	e.Ping(testUUID, Input{Kind: "success"})
	body := dashboardRequest(e, http.MethodGet, "/checks/"+e.Snapshot()[0].Key).Body.String()
	if !strings.Contains(body, ">in 5 minutes</time>") || !strings.Contains(body, `data-full="Sun, 18 Oct 2026 12:05:00 UTC"`) {
		t.Fatal("detail deadlines should use the same relative times and exact-date tooltip as the overview")
	}
	if strings.Count(body, ">now</time>") < 3 {
		t.Fatal("last success, last ping and recent events should show relative times")
	}
}

func TestDashboardPausedCheckHasNoActiveDueTimes(t *testing.T) {
	for _, elapsed := range []time.Duration{0, 65 * time.Minute} {
		e, now := newTestEngine(t)
		e.Ping(testUUID, Input{Kind: "success"})
		config := parseTestConfig(t, simpleConfig)
		config.Checks[0].Paused = true
		if err := e.Reload(config); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(elapsed)
		view := onlyCheck(t, e)
		index := dashboardRequest(e, http.MethodGet, "/").Body.String()
		if !strings.Contains(index, `<dt>Next due</dt><dd>—</dd>`) || !strings.Contains(index, `data-due=""`) {
			t.Errorf("paused after %s: overview should have no active due time or sort deadline", elapsed)
		}
		detail := dashboardRequest(e, http.MethodGet, "/checks/"+view.Key).Body.String()
		for _, label := range []string{"Next due", "Deadline"} {
			if !strings.Contains(detail, "<strong>"+label+"</strong><span>—</span>") {
				t.Errorf("paused after %s: %s should be unset", elapsed, label)
			}
		}
		if !strings.Contains(detail, `datetime="`+formatDashboardTimeAttr(view.LastSuccess)+`"`) {
			t.Fatal("pausing should preserve the displayed success history")
		}
	}
}

func TestDashboardOverdueTimesKeepHistoryRelative(t *testing.T) {
	e, now := newTestEngine(t)
	e.Ping(testUUID, Input{Kind: "success"})
	*now = now.Add(65 * time.Minute)
	index := dashboardRequest(e, http.MethodGet, "/").Body.String()
	if !strings.Contains(index, ">1 hour overdue</time>") || !strings.Contains(index, ">1 hour ago</time>") {
		t.Fatal("overview should distinguish an overdue ping from historical success")
	}
	detail := dashboardRequest(e, http.MethodGet, "/checks/"+onlyCheck(t, e).Key).Body.String()
	for _, want := range []string{">1 hour overdue</time>", ">59 minutes overdue</time>", ">1 hour ago</time>"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q", want)
		}
	}
}

func TestDashboardNotFoundKeepsSiteIdentityAndThemeControls(t *testing.T) {
	e := dashboardEngine(t)
	e.applied.SiteName = "Example status"
	path := "/checks/missing"
	w := dashboardRequest(e, http.MethodGet, path)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<title>Check not found · Example status</title>",
		`aria-label="Theme"`,
		`aria-label="System theme"`,
		`aria-label="Light theme"`,
		`aria-label="Dark theme"`,
		`href="/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("404 page missing %q", want)
		}
	}
	head := dashboardRequest(e, http.MethodHead, path)
	if head.Code != http.StatusNotFound || head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") {
		t.Fatal("404 HEAD should retain GET status and length without a response body")
	}
}

func TestDashboardDescriptionIsOptionalPlainTextAndReloads(t *testing.T) {
	e, _ := newTestEngine(t)
	key := onlyCheck(t, e).Key
	config := parseTestConfig(t, strings.Replace(simpleConfig, "    every:", "    description: |\n      Back up <b>all databases</b> & verify.\n      Contact: ops@example.test\n    every:", 1))
	e.Ping(testUUID, Input{Kind: "success"})
	if err := e.Reload(config); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/checks/" + key} {
		body := dashboardRequest(e, http.MethodGet, path).Body.String()
		if !strings.Contains(body, "Back up &lt;b&gt;all databases&lt;/b&gt; &amp; verify.") || !strings.Contains(body, "Contact: ops@example.test") || strings.Contains(body, "<b>all databases</b>") {
			t.Fatalf("%s: description must be visible as escaped plain text", path)
		}
		if path == "/" && !strings.Contains(body, `aria-describedby="description-`+key+`"`) {
			t.Fatal("check name must expose its description through the styled tooltip")
		}
	}
	if got := onlyCheck(t, e); got.Key != key || got.Status != "up" || len(got.Events) != 1 {
		t.Fatal("description reload reset identity or runtime history")
	}
	if err := e.Reload(parseTestConfig(t, simpleConfig)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/checks/" + key} {
		body := dashboardRequest(e, http.MethodGet, path).Body.String()
		if strings.Contains(body, "Contact: ops@example.test") {
			t.Fatalf("%s: removed description still displayed", path)
		}
	}
}

func TestDashboardEventHistoryHasReadableOutcomesAndNewestFirst(t *testing.T) {
	e, now := newTestEngine(t)
	e.Ping(testUUID, Input{Kind: "start", RID: testUUID})
	*now = now.Add(2 * time.Second)
	code := 2
	e.Ping(testUUID, Input{Kind: "fail", RID: testUUID, ExitCode: &code, Body: "connection refused"})
	*now = now.Add(time.Minute)
	e.Ping(testUUID, Input{Kind: "start", RID: otherUUID})
	*now = now.Add(4 * time.Second)
	e.Ping(testUUID, Input{Kind: "success", RID: otherUUID, Body: "retry succeeded"})
	body := dashboardRequest(e, http.MethodGet, "/checks/"+onlyCheck(t, e).Key).Body.String()
	for _, want := range []string{"Event history", "Succeeded", "Failed", "Started", "Duration: 4s", "Exit code: 2", "cleared on restart"} {
		if !strings.Contains(body, want) {
			t.Errorf("event history missing %q", want)
		}
	}
	if strings.Index(body, "retry succeeded") > strings.Index(body, "connection refused") {
		t.Fatal("history must show newest events first")
	}
}
