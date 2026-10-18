package healthchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newWebhookTestEngine(t *testing.T, endpoint string) *Engine {
	t.Helper()
	config := fmt.Sprintf(`public_url: https://status.example.test
webhook:
  name: test
  url: %q
checks:
  - uuid: %s
    name: Heartbeat
    tags: critical api
    every: 5m
    grace: 1m
    notify: true
    region: az1
    depends_on_regions: [az2]
`, endpoint, testUUID)
	return NewEngine(parseTestConfig(t, config), time.Now)
}

func startWebhookWorker(t *testing.T, e *Engine) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.RunWebhook(ctx)
	}()
	return cancel, done
}

func waitFor(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func receiveWebhookEvent(t *testing.T, events <-chan WebhookEvent) WebhookEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook event")
		return WebhookEvent{}
	}
}

func receiveWebhookStatus(t *testing.T, statuses <-chan string) string {
	t.Helper()
	select {
	case status := <-statuses:
		return status
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook status")
		return ""
	}
}

func TestWebhookWorkerSkipsInitialRecoveryAndCoalescesDownUp(t *testing.T) {
	events := make(chan WebhookEvent, 4)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s with Content-Type %q", r.Method, r.Header.Get("Content-Type"))
		}
		var event WebhookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode webhook: %v", err)
		}
		events <- event
		if requests.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL+"?token=secret-value")
	cancel, done := startWebhookWorker(t, e)
	defer func() { cancel(); <-done }()

	select {
	case event := <-events:
		t.Fatalf("initial status was delivered: %q", event.Status)
	case <-time.After(75 * time.Millisecond):
	}

	e.Ping(testUUID, Input{Kind: "fail"})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("down webhook did not start")
	}
	e.Ping(testUUID, Input{Kind: "success"})
	close(releaseFirst)

	got := []WebhookEvent{receiveWebhookEvent(t, events), receiveWebhookEvent(t, events)}
	if got[0].Status != "down" || got[1].Status != "up" {
		t.Fatalf("delivered statuses = [%s %s], want [down up]", got[0].Status, got[1].Status)
	}
	if got[0].UniqueKey == "" || got[0].URL != "https://status.example.test/checks/"+got[0].UniqueKey {
		t.Fatalf("event detail URL does not use unique_key: %+v", got[0])
	}
	encodedEvent := string(mustJSON(t, got[0]))
	if strings.Contains(encodedEvent, testUUID) || strings.Contains(encodedEvent, "secret-value") {
		t.Fatal("webhook payload disclosed the check UUID or target URL credentials")
	}
	waitFor(t, "recovery state to be recorded", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.checks[testUUID].notifiedDown && e.checks[testUUID].flight == nil
	})
}

func TestWebhookNotifiesOncePerOutageAndOnlyRecoversAfterSuccessfulDown(t *testing.T) {
	statuses := make(chan string, 4)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var event WebhookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode webhook: %v", err)
		}
		statuses <- event.Status
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL)
	cancel, done := startWebhookWorker(t, e)
	defer func() { cancel(); <-done }()
	e.Ping(testUUID, Input{Kind: "fail"})
	if got := receiveWebhookStatus(t, statuses); got != "down" {
		t.Fatalf("first status = %q, want down", got)
	}
	waitFor(t, "down delivery state", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.checks[testUUID].notifiedDown
	})
	for i := 0; i < 3; i++ {
		e.Ping(testUUID, Input{Kind: "fail"})
	}
	time.Sleep(50 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("repeated failure pings caused %d deliveries, want 1", got)
	}

	e.Ping(testUUID, Input{Kind: "success"})
	if got := receiveWebhookStatus(t, statuses); got != "up" {
		t.Fatalf("recovery status = %q, want up", got)
	}
	waitFor(t, "recovery delivery state", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.checks[testUUID].notifiedDown
	})
	e.Ping(testUUID, Input{Kind: "fail"})
	if got := receiveWebhookStatus(t, statuses); got != "down" {
		t.Fatalf("new outage status = %q, want down", got)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("deliveries across two outages = %d, want 3", got)
	}
}

func TestWebhookWorkerCoalescesDownUpBeforeDelivery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL)
	e.Ping(testUUID, Input{Kind: "fail"})
	e.Ping(testUUID, Input{Kind: "success"})
	cancel, done := startWebhookWorker(t, e)
	defer func() { cancel(); <-done }()
	time.Sleep(75 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want no notification for an unobserved outage", got)
	}
}

func TestWebhookWorkerRetriesServerFailureAnd429RetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		switch requests.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL)
	cancel, done := startWebhookWorker(t, e)
	defer func() { cancel(); <-done }()
	e.Ping(testUUID, Input{Kind: "fail"})

	waitFor(t, "successful retried delivery", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.checks[testUUID].notifiedDown && e.checks[testUUID].deliveryError == ""
	})
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestRetryAfterAcceptsSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, value string
		want        time.Duration
	}{
		{name: "seconds", value: "7", want: 7 * time.Second},
		{name: "date", value: now.Add(9 * time.Second).Format(http.TimeFormat), want: 9 * time.Second},
		{name: "long seconds", value: "3600", want: time.Hour},
		{name: "long date", value: now.Add(time.Hour).Format(http.TimeFormat), want: time.Hour},
		{name: "maximum seconds", value: "18446744073709551615", want: time.Duration(1<<63 - 1)},
		{name: "overflow seconds", value: "184467440737095516150", want: time.Duration(1<<63 - 1)},
		{name: "invalid", value: "tomorrow", want: -1},
		{name: "negative", value: "-5", want: -1},
		{name: "past date", value: now.Add(-time.Second).Format(http.TimeFormat), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterDelay(tc.value, now); got != tc.want {
				t.Fatalf("retryAfterDelay(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

func TestWebhookRecoveryRetrySurvivesGrace(t *testing.T) {
	for _, downAgain := range []bool{false, true} {
		t.Run(fmt.Sprintf("down_again=%v", downAgain), func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			config := strings.Replace(simpleConfig, "every: 5m", "every: 1m\n    notify: true", 1) + "webhook:\n  name: test\n  url: https://example.test/hook\n"
			e := NewEngine(parseTestConfig(t, config), func() time.Time { return now })
			e.Ping(testUUID, Input{Kind: "fail"})
			d, _ := e.prepareDelivery(context.Background())
			if d == nil || d.status != "down" {
				t.Fatal("missing outage delivery")
			}
			e.finishDelivery(d, http.StatusNoContent, "", nil)
			e.Ping(testUUID, Input{Kind: "success"})
			d, _ = e.prepareDelivery(context.Background())
			if d == nil || d.status != "up" {
				t.Fatal("missing recovery delivery")
			}
			e.finishDelivery(d, http.StatusServiceUnavailable, "90", nil)
			now = now.Add(time.Minute)
			view := onlyCheck(t, e)
			if view.Status != "grace" || view.DeliveryError == "" {
				t.Fatalf("grace lost pending recovery error: %+v", view)
			}
			if next, wait := e.prepareDelivery(context.Background()); next != nil || wait != 30*time.Second {
				t.Fatalf("recovery retried before deadline: delivery=%v wait=%s", next != nil, wait)
			}
			now = now.Add(30 * time.Second)
			if downAgain {
				e.Ping(testUUID, Input{Kind: "fail"})
			}
			d, _ = e.prepareDelivery(context.Background())
			if downAgain {
				if d != nil {
					t.Fatal("sent obsolete recovery after a new failure")
				}
				return
			}
			if d == nil || d.status != "up" {
				t.Fatal("recovery was not retried during grace")
			}
			e.finishDelivery(d, http.StatusNoContent, "", nil)
			if view := onlyCheck(t, e); view.DeliveryError != "" {
				t.Fatalf("successful recovery retained delivery error: %s", view.DeliveryError)
			}
			if next, _ := e.prepareDelivery(context.Background()); next != nil {
				t.Fatal("repeated delivered recovery")
			}
		})
	}
}

func TestWebhookDoesNotRetryBeforeReceiverDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	checks := strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    notify: true", 1)
	second := strings.Replace(strings.Replace(checks, testUUID, otherUUID, 1), "Heartbeat", "Second check", 1)
	config := checks + strings.Split(second, "checks:\n")[1] + "webhook:\n  name: fixture\n  url: https://example.test/hook\n"
	c := parseTestConfig(t, config)
	e := NewEngine(c, func() time.Time { return now })
	e.Ping(testUUID, Input{Kind: "fail"})
	e.Ping(otherUUID, Input{Kind: "fail"})
	delivery, _ := e.prepareDelivery(context.Background())
	if delivery == nil {
		t.Fatal("missing initial outage delivery")
	}
	e.finishDelivery(delivery, http.StatusTooManyRequests, "3600", nil)
	if next, wait := e.prepareDelivery(context.Background()); next != nil || wait != time.Hour {
		t.Fatalf("another check bypassed the receiver deadline: delivery=%v wait=%s", next != nil, wait)
	}
	now = now.Add(5 * time.Minute)
	if next, wait := e.prepareDelivery(context.Background()); next != nil || wait != 55*time.Minute {
		t.Fatalf("after five minutes: delivery=%v wait=%s", next != nil, wait)
	}
	e.Reload(parseTestConfig(t, strings.Replace(config, "name: Heartbeat", "name: Renamed", 1)))
	if next, wait := e.prepareDelivery(context.Background()); next != nil || wait != 55*time.Minute {
		t.Fatalf("metadata reload bypassed the receiver deadline: delivery=%v wait=%s", next != nil, wait)
	}
	now = now.Add(55 * time.Minute)
	next, _ := e.prepareDelivery(context.Background())
	if next == nil {
		t.Fatal("retry did not become ready at the receiver deadline")
	}
	e.finishDelivery(next, http.StatusTooManyRequests, "3600", nil)
	e.Reload(parseTestConfig(t, strings.Replace(config, "https://example.test/hook", "https://other.example.test/hook", 1)))
	next, _ = e.prepareDelivery(context.Background())
	if next == nil {
		t.Fatal("a different webhook URL retained the old receiver's deadline")
	}
	next.cancel()
}

func TestWebhookMetadataReloadPreservesDeliveredOutage(t *testing.T) {
	for _, change := range []string{"name", "body"} {
		for _, recovered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovered=%v", change, recovered), func(t *testing.T) {
				e := newWebhookTestEngine(t, "https://example.test/hook")
				e.Ping(testUUID, Input{Kind: "fail"})
				d, _ := e.prepareDelivery(context.Background())
				if d == nil {
					t.Fatal("missing outage delivery")
				}
				e.finishDelivery(d, http.StatusNoContent, "", nil)
				if recovered {
					e.Ping(testUUID, Input{Kind: "success"})
				}
				name, extra := "test", ""
				if change == "name" {
					name = "renamed"
				} else {
					extra = "  body: '{\"content\": {{ json .message }}}'\n"
				}
				config := strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    notify: true", 1) + "webhook:\n  name: " + name + "\n  url: https://example.test/hook\n" + extra
				e.Reload(parseTestConfig(t, config))
				d, _ = e.prepareDelivery(context.Background())
				if recovered {
					if d == nil || d.status != "up" {
						t.Fatal("metadata edit lost pending recovery")
					}
					e.finishDelivery(d, http.StatusNoContent, "", nil)
				} else if d != nil {
					d.cancel()
					t.Fatal("metadata edit repeated an already delivered outage")
				}
			})
		}
	}
}

func TestWebhookBodyReloadKeepsInFlightOutcome(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e := newWebhookTestEngine(t, "https://example.test/hook")
			e.Ping(testUUID, Input{Kind: "fail"})
			d, _ := e.prepareDelivery(context.Background())
			if d == nil {
				t.Fatal("missing outage delivery")
			}
			config := strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    notify: true", 1) + "webhook:\n  name: test\n  url: https://example.test/hook\n  body: '{\"content\": {{ json .message }}}'\n"
			e.Reload(parseTestConfig(t, config))
			e.finishDelivery(d, status, "", nil)
			next, _ := e.prepareDelivery(context.Background())
			if status == http.StatusNoContent {
				if next != nil {
					next.cancel()
					t.Fatal("body reload discarded a successful in-flight delivery")
				}
				e.Ping(testUUID, Input{Kind: "success"})
				next, _ = e.prepareDelivery(context.Background())
				if next == nil || next.status != "up" {
					t.Fatal("body reload lost in-flight outage's recovery")
				}
			} else if next == nil {
				t.Fatal("old payload failure prevented delivery with the new template")
			}
			next.cancel()
		})
	}
}

func TestRemovedCheckResponsePreservesReceiverPause(t *testing.T) {
	for _, outcome := range []string{"same URL", "different URL", "canceled request"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			checks := strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    notify: true", 1)
			second := strings.Replace(strings.Replace(checks, testUUID, otherUUID, 1), "Heartbeat", "Second", 1)
			webhook := "webhook:\n  name: fixture\n  url: https://example.test/hook\n"
			e := NewEngine(parseTestConfig(t, checks+strings.Split(second, "checks:\n")[1]+webhook), func() time.Time { return now })
			e.Ping(testUUID, Input{Kind: "fail"})
			e.Ping(otherUUID, Input{Kind: "fail"})
			d, _ := e.prepareDelivery(context.Background())
			if d == nil {
				t.Fatal("missing outage delivery")
			}
			remaining := otherUUID
			if d.checkID == otherUUID {
				remaining = testUUID
			}
			if outcome == "different URL" {
				webhook = strings.Replace(webhook, "example.test", "other.example.test", 1)
			}
			e.Reload(parseTestConfig(t, strings.Replace(checks, testUUID, remaining, 1)+webhook))
			status, requestErr := http.StatusTooManyRequests, error(nil)
			if outcome == "canceled request" {
				status, requestErr = 0, context.Canceled
			}
			// The HTTP response has completed; reload won the lock before finishDelivery.
			e.finishDelivery(d, status, "3600", requestErr)
			next, wait := e.prepareDelivery(context.Background())
			if outcome == "same URL" {
				if next != nil || wait != time.Hour {
					if next != nil {
						next.cancel()
					}
					t.Fatalf("removed check lost receiver pause: delivery=%v wait=%s", next != nil, wait)
				}
			} else {
				if next == nil {
					t.Fatal("old receiver response or canceled request paused current delivery")
				}
				next.cancel()
			}
		})
	}
}

func TestWebhookPermanentClientErrorIsVisibleAndDoesNotRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("private response body"))
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL+"?token=secret-value")
	cancel, done := startWebhookWorker(t, e)
	defer func() { cancel(); <-done }()
	e.Ping(testUUID, Input{Kind: "fail"})

	waitFor(t, "permanent delivery error", func() bool {
		return onlyCheck(t, e).DeliveryError != ""
	})
	if got := onlyCheck(t, e).DeliveryError; !strings.Contains(got, "400") || strings.Contains(got, "secret-value") || strings.Contains(got, "private response") {
		t.Fatalf("unsafe or unhelpful delivery error: %q", got)
	}
	for i := 0; i < 3; i++ {
		e.Ping(testUUID, Input{Kind: "log"})
	}
	e.Ping(testUUID, Input{Kind: "success"})
	time.Sleep(50 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d after permanent failure, want 1", got)
	}
}

func TestWebhookInFlightDoesNotBlockPing(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL)
	cancel, done := startWebhookWorker(t, e)
	defer func() { cancel(); <-done }()
	e.Ping(testUUID, Input{Kind: "fail"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("webhook did not start")
	}

	pingDone := make(chan struct{})
	go func() {
		e.Ping(testUUID, Input{Kind: "log", Body: "still responsive"})
		close(pingDone)
	}()
	select {
	case <-pingDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Ping blocked behind webhook network request")
	}
	close(release)
}

func TestWebhookContextCancellationInterruptsRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		close(requestStarted)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()
	e := newWebhookTestEngine(t, server.URL)
	cancel, done := startWebhookWorker(t, e)
	e.Ping(testUUID, Input{Kind: "fail"})
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestWebhookMessageIsLimitedByUnicodeCharacters(t *testing.T) {
	message := limitRunes(strings.Repeat("界", maxWebhookMessageRunes+8), maxWebhookMessageRunes)
	if got := len([]rune(message)); got != maxWebhookMessageRunes {
		t.Fatalf("message rune count = %d, want %d", got, maxWebhookMessageRunes)
	}
}

func mustJSON(t *testing.T, event WebhookEvent) []byte {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
