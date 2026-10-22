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
// real handler and CSP; only the 15-second interval is driven manually.
func TestDashboardPartialRefreshBrowser(t *testing.T) {
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
	var failure atomic.Int32
	var heldRequests atomic.Int32
	requestStarted := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	handler := healthchecks.NewHandler(e, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch failure.Load() {
		case 1:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>Sign in</body></html>")
		case 3:
			http.Redirect(w, r, "/login", http.StatusFound)
		case 4:
			heldRequests.Add(1)
			select {
			case requestStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseRequest:
				handler.ServeHTTP(w, r)
			case <-r.Context().Done():
			}
		case 5:
			<-r.Context().Done()
		default:
			handler.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	initFile := filepath.Join(t.TempDir(), "clock.js")
	if err := os.WriteFile(initFile, []byte(`
const realInterval = window.setInterval.bind(window);
window.setInterval = (callback, delay, ...args) => {
  if (delay === 15000) { window.refreshForTest = callback; return 0; }
  return realInterval(callback, delay, ...args);
};
window.documentIdentity = {};
window.cspErrors = [];
document.addEventListener("securitypolicyviolation", event => window.cspErrors.push(event.violatedDirective));
`), 0600); err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("hc-refresh-%d", time.Now().UnixNano())
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
	refresh := func(assertions string) {
		t.Helper()
		eval("await window.refreshForTest();\n" + assertions)
	}
	browser("", "--init-script", initFile, "open", server.URL)
	eval(`
window.originalDocument = window.documentIdentity;
document.querySelector('[data-theme-choice="dark"]').click();
document.querySelector('[data-filter="up"]').click();
const sort = document.querySelector('#sort-select');
sort.value = 'name'; sort.dispatchEvent(new Event('change'));
document.querySelector('#sort-direction').click();
window.card = document.querySelector('.check-card[data-name="Backup"]');
window.link = window.card.querySelector('a'); window.link.focus();
`)
	eval(`
// A client clock ahead of the server must not make every refresh rewrite
// unchanged timestamps with server text and then immediately rewrite it again.
const realNow = Date.now;
const clientNow = realNow() + 65000;
Date.now = () => clientNow;
const timestamp = document.querySelector('time');
// Run the regular timestamp updater with the adjusted client clock.
await new Promise(resolve => setTimeout(resolve, 1100));
const changes = [];
const observer = new MutationObserver(records => changes.push(...records));
observer.observe(timestamp, { childList: true, characterData: true, subtree: true });
await window.refreshForTest();
changes.push(...observer.takeRecords());
observer.disconnect();
Date.now = realNow;
assert(changes.length === 0, 'refresh rewrote an unchanged timestamp');
`)
	e.Ping(uuid, healthchecks.Input{Kind: "fail", Body: "failure log"})
	e.SetReloadError("config <invalid>")
	refresh(`
assert(window.documentIdentity === window.originalDocument, 'document was reloaded');
assert(window.card.isConnected && window.card.dataset.status === 'down', 'card did not update in place');
assert(document.activeElement === window.link, 'focused link was lost');
assert(window.card.hidden, 'active filter was lost');
assert(document.querySelector('[data-filter="down"] .metric-value').textContent === '1', 'counter is stale');
assert(document.querySelector('#sort-select').value === 'name', 'sort criterion was lost');
assert(document.querySelector('#sort-direction').dataset.direction === 'desc', 'sort direction was lost');
assert(document.documentElement.dataset.theme === 'dark', 'theme was lost');
assert(document.body.textContent.includes('config <invalid>'), 'reload error is stale');
document.querySelector('[data-filter="all"]').click();
`)
	e.Ping(uuid, healthchecks.Input{Kind: "success"})
	refresh(`
const sort = document.querySelector('#sort-select');
sort.value = 'state'; sort.dispatchEvent(new Event('change'));
document.querySelector('#sort-direction').click();
window.link.focus();
const range = document.createRange(); range.selectNodeContents(window.link);
document.getSelection().removeAllRanges(); document.getSelection().addRange(range);
assert(document.querySelector('.check-card').dataset.name === 'Queue', 'state sort fixture is incorrect');
`)
	e.Ping(uuid, healthchecks.Input{Kind: "fail", Body: "failure log"})
	refresh(`
assert(document.querySelector('.check-card') === window.card, 'status change did not reorder cards');
assert(document.activeElement === window.link, 'live reorder lost keyboard focus');
assert(document.getSelection().toString() === 'Backup', 'live reorder lost text selection');
`)
	browser("", "set", "viewport", "390", "844")
	eval(`
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
window.originalDocument = window.documentIdentity;
window.log = [...document.querySelectorAll('details')].find(node => node.textContent.includes('original log'));
window.log.open = true;
window.summary = window.log.querySelector('summary'); window.summary.focus();
window.log.querySelector('pre').scrollTop = 100;
assert(window.log.querySelector('pre').scrollTop === 100, 'log scroll fixture is incorrect');
window.scrollTo(0, 200);
window.savedScroll = window.scrollY;
assert(window.savedScroll > 0, 'window scroll fixture is incorrect');
const text = window.log.querySelector('pre').firstChild;
document.getSelection().setBaseAndExtent(text, 0, text, 8);
`)
	// Same timestamp and same contents still represent distinct events.
	e.Ping(uuid, healthchecks.Input{Kind: "log", Body: `<img src=x onerror=alert(1)>`})
	e.Ping(uuid, healthchecks.Input{Kind: "log", Body: `<img src=x onerror=alert(1)>`})
	refresh(`
assert(window.documentIdentity === window.originalDocument, 'detail document was reloaded');
assert(document.querySelectorAll('.event').length === 6, 'new events did not arrive while log was open');
assert(window.log.isConnected && window.log.open, 'expanded log was replaced or closed');
assert(window.log.textContent.includes('original log'), 'expanded log changed identity');
assert(document.activeElement === window.summary, 'log focus was lost');
assert(window.log.querySelector('pre').scrollTop === 100, 'log scroll position was lost');
assert(window.scrollY === window.savedScroll, 'window scroll position was lost');
assert(document.getSelection().toString() === 'original', 'selected log text was lost');
assert(!document.querySelector('#main img'), 'log HTML was executed');
assert(document.querySelector('.event pre').textContent === '<img src=x onerror=alert(1)>', 'log text changed');
`)
	failure.Store(1)
	refresh(`
assert(!document.querySelector('#refresh-error').hidden, 'network failure is invisible');
assert(window.log.isConnected && window.log.open, 'network failure destroyed the current view');
`)
	failure.Store(2)
	refresh(`
assert(!document.querySelector('#refresh-error').hidden, 'unexpected HTML was accepted');
assert(window.log.isConnected, 'login page replaced dashboard');
`)
	failure.Store(3)
	refresh(`
assert(!document.querySelector('#refresh-error').hidden, 'redirect was accepted');
assert(window.log.isConnected && location.pathname.startsWith('/checks/'), 'redirect replaced dashboard');
`)
	failure.Store(5)
	refresh(`
assert(!document.querySelector('#refresh-error').hidden, 'request timeout is invisible');
assert(window.log.isConnected && window.log.open, 'timeout destroyed the view');
`)
	failure.Store(4)
	eval(`window.pendingRefresh = window.refreshForTest()`)
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh request was not started")
	}
	refresh("")
	if heldRequests.Load() != 1 {
		t.Fatalf("overlapping refreshes sent %d requests, want 1", heldRequests.Load())
	}
	close(releaseRequest)
	eval(`await window.pendingRefresh`)
	failure.Store(0)
	e.SetReloadError("")
	e.Ping(uuid, healthchecks.Input{Kind: "success"})
	refresh(`
assert(document.querySelector('#refresh-error').hidden, 'refresh did not recover');
assert(document.querySelector('.detail-status .badge').textContent === 'up', 'detail status is stale');
assert(window.log.isConnected && window.log.open, 'recovery closed the log');
`)
	if err := e.Reload(parse(strings.Replace(config, "name: Backup", "name: Renamed", 1))); err != nil {
		t.Fatal(err)
	}
	refresh(`
assert(document.querySelector('h1').textContent === 'Renamed', 'renamed check heading is stale');
assert(document.title.includes('Renamed'), 'title is stale');
`)
	if err := e.Reload(parse(queueOnly)); err != nil {
		t.Fatal(err)
	}
	refresh(`
assert(document.querySelector('h1').textContent === 'Check not found', 'removed check is still shown');
assert(document.querySelector('#refresh-error').hidden, 'valid missing-check response was rejected');
`)
	if err := e.Reload(parse(config)); err != nil {
		t.Fatal(err)
	}
	refresh(`
assert(document.querySelector('h1').textContent === 'Backup', 'restored check did not reappear');
assert(document.querySelector('.detail-status .badge'), 'restored detail status is missing');
assert(!document.querySelector('.theme-switch'), 'restored detail page shows theme controls');
assert(document.documentElement.dataset.theme === 'dark', 'restored detail page lost the saved theme');
`)
	browser("", "open", server.URL)
	eval(`
document.querySelector('[data-theme-choice="light"]').click();
assert(document.documentElement.dataset.theme === 'light', 'overview theme controls stopped working');
`)
	if err := e.Reload(parse(queueOnly)); err != nil {
		t.Fatal(err)
	}
	refresh(`
assert(document.querySelectorAll('.check-card').length === 1, 'removed card remains on overview');
`)
	if err := e.Reload(parse(config)); err != nil {
		t.Fatal(err)
	}
	refresh(`
assert(document.querySelectorAll('.check-card').length === 2, 'new checks did not appear');
const link = document.querySelector('.check-card[data-name="Backup"] a');
link.focus();
const tooltip = document.getElementById(link.getAttribute('aria-describedby'));
await new Promise(resolve => setTimeout(resolve, 350));
assert(getComputedStyle(tooltip).visibility === 'visible', 'new description tooltip is invisible');
const bounds = tooltip.getBoundingClientRect();
assert(bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight, 'new tooltip is outside the viewport');
link.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
// Description tooltips retain their existing 120 ms hide delay.
await new Promise(resolve => setTimeout(resolve, 200));
assert(getComputedStyle(tooltip).visibility === 'hidden', 'new tooltip does not support Escape');
document.querySelector('[data-filter="up"]').click();
assert([...document.querySelectorAll('.check-card')].every(card => card.hidden), 'filter stopped working after adding checks');
assert(!document.querySelector('#filter-empty').hidden, 'empty filter message is missing');
`)
	if directory := os.Getenv("HC_BROWSER_ARTIFACT_DIR"); directory != "" {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		eval(`document.querySelector('[data-filter="all"]').click()`)
		t.Log(browser("", "screenshot", filepath.Join(directory, "dashboard-refresh.png")))
	}
}
