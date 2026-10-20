package healthchecks

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type handler struct {
	engine     *Engine
	metricsKey string
	pingSlots  chan struct{}
}

func NewHandler(e *Engine, metricsKey string) http.Handler {
	return &handler{engine: e, metricsKey: metricsKey, pingSlots: make(chan struct{}, 64)}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasPrefix(r.URL.Path, "/ping/"):
		h.ping(w, r)
	case r.URL.Path == "/metrics":
		h.metrics(w, r)
	case r.URL.Path == "/api/v3/status/" || r.URL.Path == "/healthz":
		if !readMethod(w, r) {
			return
		}
		if r.URL.Path != "/healthz" && h.engine.Ready() != nil {
			respond(w, r, 503, "database unavailable")
			return
		}
		respond(w, r, 200, "OK")
	default:
		renderDashboard(w, r, h.engine)
	}
}

func respond(w http.ResponseWriter, r *http.Request, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		_, _ = io.WriteString(w, body)
	}
}

func readMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == "GET" || r.Method == "HEAD" {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	respond(w, r, 405, "method not allowed")
	return false
}

func (h *handler) ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "POST" {
		w.Header().Set("Allow", "GET, HEAD, POST")
		respond(w, r, 405, "method not allowed")
		return
	}
	select {
	case h.pingSlots <- struct{}{}:
		defer func() { <-h.pingSlots }()
	default:
		respond(w, r, 503, "busy")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/ping/"), "/")
	if len(parts) > 2 || !uuidPattern.MatchString(parts[0]) {
		respond(w, r, 404, "not found")
		return
	}
	h.engine.mu.Lock()
	exists := h.engine.checks[parts[0]] != nil
	h.engine.mu.Unlock()
	if !exists {
		respond(w, r, 404, "not found")
		return
	}
	in := Input{Kind: "success"}
	if len(parts) == 2 {
		switch parts[1] {
		case "":
		case "start", "fail", "log":
			in.Kind = parts[1]
		default:
			if parts[1] == "" || strings.IndexFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				respond(w, r, 404, "not found")
				return
			}
			n, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil || n > 255 {
				respond(w, r, 400, "invalid url format")
				return
			}
			code := int(n)
			in.ExitCode = &code
			if n != 0 {
				in.Kind = "fail"
			}
		}
	}
	in.RID = r.URL.Query().Get("rid")
	if _, present := r.URL.Query()["rid"]; present && !uuidPattern.MatchString(in.RID) {
		respond(w, r, 400, "invalid uuid format")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, BodyLimit+1))
	if err != nil {
		respond(w, r, 400, "could not read request body")
		return
	}
	if len(body) > BodyLimit {
		body = body[:BodyLimit]
		in.Truncated = true
	}
	in.Body = string(body)
	found, err := h.engine.Ping(parts[0], in)
	if err != nil {
		respond(w, r, 503, "state could not be saved")
		return
	}
	if !found {
		respond(w, r, 404, "not found")
		return
	}
	w.Header().Set("Ping-Body-Limit", strconv.Itoa(BodyLimit))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	respond(w, r, 200, "OK")
}

func (h *handler) metrics(w http.ResponseWriter, r *http.Request) {
	if !readMethod(w, r) {
		return
	}
	if h.metricsKey != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			respond(w, r, 401, "bearer token required")
			return
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(h.metricsKey)) != 1 {
			respond(w, r, 403, "forbidden")
			return
		}
	}
	views, err := h.engine.snapshot()
	if err != nil {
		respond(w, r, 503, "state could not be refreshed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if r.Method == "HEAD" {
		return
	}
	_, _ = io.WriteString(w, "# HELP hc_check_up Whether the check is currently up (1 for yes, 0 for no).\n# TYPE hc_check_up gauge\n")
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	for _, c := range views {
		value := 1
		if c.Status == "down" {
			value = 0
		}
		sort.Strings(c.DependsOnRegions)
		_, _ = fmt.Fprintf(w, "hc_check_up{name=\"%s\",unique_key=\"%s\",depends_on_regions=\"%s\"} %d\n", escape.Replace(c.Name), c.Key, strings.Join(c.DependsOnRegions, ","), value)
	}
}
