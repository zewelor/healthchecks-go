package healthchecks

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	if w.Code != 200 || !strings.Contains(w.Body.String(), "} 0\n") || strings.Contains(w.Body.String(), testUUID) {
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
