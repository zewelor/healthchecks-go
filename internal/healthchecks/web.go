package healthchecks

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type dashboardData struct {
	Info             ServerView
	Checks           []CheckView
	Current          CheckView
	Total, Up        int
	Down, Grace      int
	Awaiting, Paused int
	HasCurrent       bool
	NotFound         bool
	Style            template.CSS
	Script           template.JS
	ThemeScript      template.JS
	Now              time.Time
}

type dashboardTimestamp struct {
	Value, Now time.Time
	Due        bool
}

//go:embed web/dashboard.html
var dashboardHTML string

//go:embed web/dashboard.css
var dashboardStyles string

//go:embed web/dashboard.js
var dashboardScript string

//go:embed web/theme.js
var dashboardThemeScript string

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"eventLabel": func(kind string) string {
		switch kind {
		case "success":
			return "Succeeded"
		case "fail":
			return "Failed"
		case "start":
			return "Started"
		case "log":
			return "Log received"
		case "ignored":
			return "Ignored while paused"
		default:
			return kind
		}
	},
	"formatTime":   formatDashboardTime,
	"relativeTime": formatDashboardRelativeTime,
	"dueTime":      formatDashboardDueTime,
	"timestamp":    func(value, now time.Time) dashboardTimestamp { return dashboardTimestamp{Value: value, Now: now} },
	"dueTimestamp": func(value, now time.Time, status string) dashboardTimestamp {
		if status == "paused" {
			value = time.Time{}
		}
		return dashboardTimestamp{Value: value, Now: now, Due: true}
	},
	"timeAttr":       formatDashboardTimeAttr,
	"formatDuration": formatDashboardDuration,
	"statusClass": func(status string) string {
		switch status {
		case "up", "down", "grace", "awaiting", "paused":
			return status
		default:
			return "unknown"
		}
	},
}).Parse(dashboardHTML))

func renderDashboard(w http.ResponseWriter, r *http.Request, e *Engine) {
	setDashboardHeaders(w)
	data := dashboardData{Style: template.CSS(dashboardStyles), Script: template.JS(dashboardScript), ThemeScript: template.JS(dashboardThemeScript), Now: e.now()}
	if r.URL.EscapedPath() != r.URL.Path || (r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/checks/")) {
		data.Info = e.Info()
		renderDashboardNotFound(w, r, data)
		return
	}

	views := e.Snapshot()
	data.Info = e.Info()
	data.Checks, data.Total = views, len(views)
	if r.URL.Path != "/" {
		key := strings.TrimPrefix(r.URL.Path, "/checks/")
		if key == "" || strings.Contains(key, "/") {
			renderDashboardNotFound(w, r, data)
			return
		}
		found := false
		for _, view := range views {
			if view.Key == key {
				data.Current = view
				data.HasCurrent = true
				found = true
				break
			}
		}
		if !found {
			renderDashboardNotFound(w, r, data)
			return
		}
	}
	for _, view := range views {
		switch view.Status {
		case "up":
			data.Up++
		case "down":
			data.Down++
		case "grace":
			data.Grace++
		case "paused":
			data.Paused++
		default:
			data.Awaiting++
		}
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeDashboardError(w, r, http.StatusMethodNotAllowed, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Method not allowed</title></head><body><h1>Method not allowed</h1><p>Use GET or HEAD.</p></body></html>`)
		return
	}
	renderDashboardPage(w, r, http.StatusOK, data)
}

func renderDashboardNotFound(w http.ResponseWriter, r *http.Request, data dashboardData) {
	data.NotFound = true
	renderDashboardPage(w, r, http.StatusNotFound, data)
}

func renderDashboardPage(w http.ResponseWriter, r *http.Request, status int, data dashboardData) {
	var body bytes.Buffer
	if err := dashboardTemplate.Execute(&body, data); err != nil {
		writeDashboardError(w, r, http.StatusInternalServerError, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Dashboard unavailable</title></head><body><h1>Dashboard unavailable</h1></body></html>`)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func setDashboardHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	scriptHash := sha256.Sum256([]byte(dashboardScript))
	themeScriptHash := sha256.Sum256([]byte(dashboardThemeScript))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(scriptHash[:])+"' 'sha256-"+base64.StdEncoding.EncodeToString(themeScriptHash[:])+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func writeDashboardError(w http.ResponseWriter, r *http.Request, status int, body string) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

func formatDashboardTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format("Mon, 02 Jan 2006 15:04:05 MST")
}

func formatDashboardTimeAttr(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func formatDashboardDuration(value *time.Duration) string {
	if value == nil {
		return "—"
	}
	return value.Round(time.Millisecond).String()
}

func formatDashboardRelativeTime(value, now time.Time) string {
	if value.IsZero() {
		return "—"
	}
	seconds := int64(value.Sub(now) / time.Second)
	if seconds == 0 {
		return "now"
	}
	future := seconds > 0
	if !future {
		seconds = -seconds
	}
	unit, size := "second", int64(1)
	switch {
	case seconds >= 84600:
		unit, size = "day", 86400
	case seconds >= 3570:
		unit, size = "hour", 3600
	case seconds >= 60:
		unit, size = "minute", 60
	}
	amount := int64(math.Round(float64(seconds) / float64(size)))
	if amount != 1 {
		unit += "s"
	}
	if future {
		return fmt.Sprintf("in %d %s", amount, unit)
	}
	return fmt.Sprintf("%d %s ago", amount, unit)
}

func formatDashboardDueTime(value, now time.Time) string {
	text := formatDashboardRelativeTime(value, now)
	if strings.HasSuffix(text, " ago") {
		return strings.TrimSuffix(text, " ago") + " overdue"
	}
	return text
}
