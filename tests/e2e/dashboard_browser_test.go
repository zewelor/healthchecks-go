package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"healthchecks-go/internal/healthchecks"
)

// Run with HC_TEST_BROWSER=1 and agent-browser installed. The browser uses the
// real handler and CSP; interval callbacks are driven manually.
func TestDashboardManualRefreshBrowser(t *testing.T) {
	if os.Getenv("HC_TEST_BROWSER") != "1" {
		t.Skip("set HC_TEST_BROWSER=1 to run Chromium dashboard coverage")
	}
	if _, err := exec.LookPath("agent-browser"); err != nil {
		t.Fatal(err)
	}
	config := `public_url: http://localhost
checks:
  - uuid: 11111111-1111-4111-8111-111111111111
    name: Backup
    description: Initial description
    every: 5m
    grace: 1m
  - uuid: 22222222-2222-4222-8222-222222222222
    name: Queue
    every: 10m
    grace: 2m
`
	parse := func(text string) *healthchecks.Config {
		t.Helper()
		c, err := healthchecks.ParseConfig([]byte(text), func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	now := time.Now()
	initial := parse(config)
	e := healthchecks.NewEngine(initial, func() time.Time { return now })
	queueOnly := "public_url: http://localhost\nchecks:\n" + config[strings.Index(config, "  - uuid: 22222222"):]
	e.Ping(uuid, healthchecks.Input{Kind: "success", Body: "original log\n" + strings.Repeat("a line of output\n", 200)})
	key := initial.Checks[0].Key
	var dashboardRequests atomic.Int32
	handler := healthchecks.NewHandler(e, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/checks/") {
			dashboardRequests.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	initFile := filepath.Join(t.TempDir(), "clock.js")
	if err := os.WriteFile(initFile, []byte(`
const intervals = [];
window.setInterval = (callback, delay, ...args) => {
  intervals.push(() => callback(...args));
  return intervals.length;
};
window.runIntervalsForTest = async () => {
  for (const callback of intervals) await callback();
};
window.cspErrors = [];
document.addEventListener("securitypolicyviolation", event => window.cspErrors.push(event.violatedDirective));
`), 0600); err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("hc-dashboard-%d", time.Now().UnixNano())
	browser := func(input string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "agent-browser", append([]string{"--session", session}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("agent-browser %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	t.Cleanup(func() { browser("", "close") })
	eval := func(script string) {
		t.Helper()
		browser(`(async () => {
const assert = (value, message) => { if (!value) throw new Error(message); };
`+script+`;
assert(window.cspErrors.length === 0, "CSP violations: " + window.cspErrors);
return "ok";
})()`, "eval", "--stdin")
	}
	browser("", "--init-script", initFile, "open", server.URL)
	eval(`
document.querySelector('[data-theme-choice="dark"]').click();
document.querySelector('[data-filter="up"]').click();
const sort = document.querySelector('#sort-select');
sort.value = 'name'; sort.dispatchEvent(new Event('change'));
document.querySelector('#sort-direction').click();
assert(document.querySelector('.check-card[data-name="Queue"]').hidden, 'status filter does not hide awaiting checks');
assert(document.querySelector('.check-card').dataset.name === 'Queue', 'descending name sort is incorrect');
`)
	e.Ping(uuid, healthchecks.Input{Kind: "fail", Body: "failure log"})
	e.SetReloadError("config <invalid>")
	requests := dashboardRequests.Load()
	eval(`
await window.runIntervalsForTest();
assert(document.querySelector('.check-card[data-name="Backup"]').dataset.status === 'up', 'overview updated without a manual reload');
assert(document.querySelector('[data-filter="down"] .metric-value').textContent === '0', 'counter updated without a manual reload');
assert(!document.body.textContent.includes('config <invalid>'), 'reload error arrived without a manual reload');
`)
	if got := dashboardRequests.Load(); got != requests {
		t.Fatalf("overview sent %d background requests, want 0", got-requests)
	}
	browser("", "open", server.URL)
	eval(`
assert(document.querySelector('.check-card[data-name="Backup"]').dataset.status === 'down', 'manual reload did not update the check');
assert(document.querySelector('[data-filter="down"] .metric-value').textContent === '1', 'manual reload did not update the counter');
assert(document.body.textContent.includes('config <invalid>'), 'manual reload did not show the reload error');
assert(document.querySelector('#sort-select').value === 'name', 'saved sort criterion was lost');
assert(document.querySelector('#sort-direction').dataset.direction === 'desc', 'saved sort direction was lost');
assert(document.querySelector('[data-filter="up"]').getAttribute('aria-pressed') === 'true', 'saved filter was lost');
assert(document.documentElement.dataset.theme === 'dark', 'saved theme was lost');
assert(!document.querySelector('#filter-empty').hidden, 'empty filter message is missing');
document.querySelector('[data-filter="all"]').click();
window.card = document.querySelector('.check-card[data-name="Backup"]');
window.link = window.card.querySelector('a'); window.link.focus();
const tooltip = document.getElementById(window.link.getAttribute('aria-describedby'));
await new Promise(resolve => setTimeout(resolve, 350));
assert(getComputedStyle(tooltip).visibility === 'visible', 'description tooltip is invisible');
const bounds = tooltip.getBoundingClientRect();
assert(bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight, 'description tooltip is outside the viewport');
window.link.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
await new Promise(resolve => setTimeout(resolve, 200));
assert(getComputedStyle(tooltip).visibility === 'hidden', 'description tooltip does not support Escape');
`)
	browser("", "set", "viewport", "390", "844")
	eval(`
window.link.blur();
window.card.scrollIntoView();
const bounds = window.card.getBoundingClientRect();
const target = document.elementFromPoint(bounds.right - 10, bounds.bottom - 10);
assert(target.closest('a') === window.link, 'mobile card padding does not link to details');
`)
	browser("", "click", `.check-card[data-name="Backup"]`)
	eval(`assert(location.pathname === '/checks/` + key + `', 'mobile card tap did not open details')`)
	browser("", "set", "viewport", "1280", "720")
	eval(`
assert(!document.querySelector('.theme-switch'), 'detail page shows theme controls');
assert(document.documentElement.dataset.theme === 'dark', 'detail page lost the saved theme');
assert(document.querySelectorAll('.event').length === 2, 'detail page is missing existing events');
window.log = [...document.querySelectorAll('details')].find(node => node.textContent.includes('original log'));
window.log.open = true;
window.summary = window.log.querySelector('summary'); window.summary.focus();
window.log.querySelector('pre').scrollTop = 100;
assert(window.log.querySelector('pre').scrollTop === 100, 'log scroll fixture is incorrect');
const text = window.log.querySelector('pre').firstChild;
document.getSelection().setBaseAndExtent(text, 0, text, 8);
`)
	e.Ping(uuid, healthchecks.Input{Kind: "success", Body: `<img src=x onerror=alert(1)>`})
	e.SetReloadError("")
	requests = dashboardRequests.Load()
	eval(`
await window.runIntervalsForTest();
assert(document.querySelectorAll('.event').length === 2, 'events arrived without a manual reload');
assert(document.querySelector('.detail-status .badge').textContent === 'down', 'detail status updated without a manual reload');
assert(window.log.isConnected && window.log.open, 'expanded log was closed');
assert(document.activeElement === window.summary, 'log focus was lost');
assert(window.log.querySelector('pre').scrollTop === 100, 'log scroll position was lost');
assert(document.getSelection().toString() === 'original', 'selected log text was lost');
`)
	if got := dashboardRequests.Load(); got != requests {
		t.Fatalf("detail page sent %d background requests, want 0", got-requests)
	}
	browser("", "open", server.URL+"/checks/"+key)
	eval(`
assert(document.querySelectorAll('.event').length === 3, 'manual reload did not show new events');
assert(document.querySelector('.detail-status .badge').textContent === 'up', 'manual reload did not update detail status');
assert(!document.body.textContent.includes('config <invalid>'), 'manual reload retained an old reload error');
assert(!document.querySelector('#main img'), 'log HTML was executed');
assert(document.querySelector('.event pre').textContent === '<img src=x onerror=alert(1)>', 'log text changed');
`)
	if err := e.Reload(parse(queueOnly)); err != nil {
		t.Fatal(err)
	}
	browser("", "open", server.URL+"/checks/"+key)
	eval(`assert(document.querySelector('h1').textContent === 'Check not found', 'manual reload still shows a removed check')`)
	if err := e.Reload(parse(config)); err != nil {
		t.Fatal(err)
	}
	browser("", "open", server.URL)
	eval(`
assert(document.querySelectorAll('.check-card').length === 2, 'restored checks did not reappear');
document.querySelector('[data-theme-choice="light"]').click();
assert(document.documentElement.dataset.theme === 'light', 'overview theme controls stopped working');
`)
	if directory := os.Getenv("HC_BROWSER_ARTIFACT_DIR"); directory != "" {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		t.Log(browser("", "screenshot", filepath.Join(directory, "dashboard.png")))
	}
}
