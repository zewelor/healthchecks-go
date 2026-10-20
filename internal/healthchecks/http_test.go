package healthchecks

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func capturePingLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestPingApplicationLogs(t *testing.T) {
	for _, tc := range []struct {
		method, suffix, kind, status string
		paused                       bool
		exitCode                     *int
	}{
		{method: "GET", kind: "success", status: "up"},
		{method: "HEAD", suffix: "/start", kind: "start", status: "awaiting"},
		{method: "POST", suffix: "/fail", kind: "fail", status: "down"},
		{method: "POST", suffix: "/log", kind: "log", status: "awaiting"},
		{method: "GET", suffix: "/0", kind: "success", status: "up", exitCode: new(0)},
		{method: "GET", suffix: "/7", kind: "fail", status: "down", exitCode: new(7)},
		{method: "GET", kind: "success", status: "paused", paused: true},
	} {
		t.Run(tc.method+tc.suffix+tc.status, func(t *testing.T) {
			output := capturePingLogs(t, slog.LevelInfo)
			e, _ := newTestEngine(t)
			if tc.paused {
				config := parseTestConfig(t, simpleConfig)
				config.Checks[0].Paused = true
				if err := e.Reload(config); err != nil {
					t.Fatal(err)
				}
			}
			h := NewHandler(e, "secret-token")
			w := request(h, tc.method, "/ping/"+testUUID+tc.suffix+"?rid="+otherUUID, "secret-body", "Bearer secret-token")
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			var entry map[string]any
			if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
				t.Fatalf("expected one log entry: %s (%v)", output.String(), err)
			}
			if entry["level"] != "INFO" || entry["msg"] != "Ping accepted" || entry["check"] != "Heartbeat" || entry["key"] != onlyCheck(t, e).Key || entry["kind"] != tc.kind || entry["status"] != tc.status || entry["ignored"] != tc.paused {
				t.Fatalf("unexpected ping log: %s", output.String())
			}
			if tc.exitCode != nil {
				if entry["exit_code"] != float64(*tc.exitCode) {
					t.Fatalf("missing exit code: %s", output.String())
				}
			} else if _, present := entry["exit_code"]; present {
				t.Fatal("unexpected exit code")
			}
			for _, secret := range []string{testUUID, otherUUID, "secret-body", "secret-token", "/ping/"} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("sensitive request data in logs: %s", output.String())
				}
			}
		})
	}
}

func TestWarnLevelSuppressesPingLogs(t *testing.T) {
	output := capturePingLogs(t, slog.LevelWarn)
	e, _ := newTestEngine(t)
	if got := request(NewHandler(e, ""), "POST", "/ping/"+testUUID+"/fail", "", ""); got.Code != 200 {
		t.Fatal(got.Code)
	}
	if output.Len() != 0 || onlyCheck(t, e).Status != "down" {
		t.Fatalf("WARN must suppress ping logs while still recording failure: %s", output.String())
	}
}

func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPingRoutesAndMetrics(t *testing.T) {
	e, _ := newTestEngine(t)
	h := NewHandler(e, "test-metrics-key")
	base := "/ping/" + testUUID
	for _, method := range []string{"GET", "HEAD", "POST"} {
		w := request(h, method, base+"/start?rid="+otherUUID, "starting", "")
		if w.Code != 200 || w.Header().Get("Ping-Body-Limit") != "8192" {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body.String())
		}
		if method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	w := request(h, "POST", base+"/1", "failed", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(h, "GET", "/metrics", "", "Bearer test-metrics-key")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "} 0\n") || strings.Contains(w.Body.String(), testUUID) || strings.Contains(w.Body.String(), `tags=`) {
		t.Fatalf("metrics: %d %s", w.Code, w.Body.String())
	}
	request(h, "POST", base+"/log", strings.Repeat("x", BodyLimit*3), "")
	v := onlyCheck(t, e)
	if v.Status != "down" || len(v.Events[0].Body) != BodyLimit || !v.Events[0].Truncated {
		t.Fatal("log must be bounded without changing failure")
	}
	if request(h, "HEAD", base+"/0?rid="+otherUUID, "", "").Code != 200 {
		t.Fatal("exit zero failed")
	}
	if onlyCheck(t, e).Status != "up" {
		t.Fatal("success did not recover")
	}
	if request(h, "GET", base+"/", "", "").Code != 200 {
		t.Fatal("UUID trailing slash must work")
	}
	if request(h, "GET", "/api/v3/status/", "", "").Body.String() != "OK" {
		t.Fatal("readiness failed")
	}
}

func TestPingInvalidRequestsDoNotChangeState(t *testing.T) {
	output := capturePingLogs(t, slog.LevelInfo)
	e, _ := newTestEngine(t)
	h := NewHandler(e, "key")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/ping/" + otherUUID, 404},
		{"GET", "/ping/not-a-uuid", 404},
		{"GET", "/ping/" + testUUID + "/256", 400},
		{"GET", "/ping/" + testUUID + "/start/", 404},
		{"GET", "/ping/" + testUUID + "/-1", 404},
		{"GET", "/ping/" + testUUID + "?rid=invalid", 400},
		{"PUT", "/ping/" + testUUID, 405},
	} {
		w := request(h, tc.method, tc.path, "", "")
		if w.Code != tc.status {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	if v := onlyCheck(t, e); v.Status != "awaiting" || len(v.Events) != 0 {
		t.Fatal("invalid requests changed check")
	}
	if output.Len() != 0 {
		t.Fatalf("invalid requests logged as accepted: %s", output.String())
	}
}

func TestMetricsAuthenticationAndEscaping(t *testing.T) {
	c := parseTestConfig(t, strings.Replace(simpleConfig, "Heartbeat", `'Quoted "name"'`, 1))
	e, _ := newTestEngine(t)
	e.Reload(c)
	h := NewHandler(e, "secret")
	if request(h, "GET", "/metrics", "", "").Code != 401 {
		t.Fatal("missing bearer accepted")
	}
	if request(h, "GET", "/metrics", "", "Bearer wrong").Code != 403 {
		t.Fatal("wrong bearer accepted")
	}
	w := request(h, "GET", "/metrics", "", "Bearer secret")
	if !strings.Contains(w.Body.String(), `name="Quoted \"name\""`) {
		t.Fatal(w.Body.String())
	}
}

func TestMetricsWithoutKeyAllowsAnonymousAccess(t *testing.T) {
	e, _ := newTestEngine(t)
	h := NewHandler(e, "")
	for _, method := range []string{"GET", "HEAD"} {
		for _, auth := range []string{"", "Bearer unrelated-token"} {
			t.Run(method+"/"+auth, func(t *testing.T) {
				w := request(h, method, "/metrics", "", auth)
				if w.Code != http.StatusOK || w.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("anonymous metrics: HTTP %d, headers %v", w.Code, w.Header())
				}
				if method == "GET" && !strings.Contains(w.Body.String(), "hc_check_up{") {
					t.Fatalf("missing check metric: %s", w.Body.String())
				}
				if method == "HEAD" && w.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
			})
		}
	}
}
