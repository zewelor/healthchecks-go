package healthchecks

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
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
	Script           template.JS
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"formatTime":     formatDashboardTime,
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
	"region": func(value string) string {
		if value == "" {
			return "Not set"
		}
		return value
	},
}).Parse(dashboardHTML))

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{if .HasCurrent}}{{.Current.Name}} · healthchecks-go{{else}}healthchecks-go · Health checks{{end}}</title>
  <style>
    :root { color-scheme: dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #020617; color: #e2e8f0; font-synthesis: none; }
    * { box-sizing: border-box; }
    body { margin: 0; min-width: 320px; background: #020617; color: #e2e8f0; line-height: 1.5; }
    a { color: #7dd3fc; text-decoration: none; }
    a:hover { color: #bae6fd; text-decoration: underline; }
    a:focus-visible, summary:focus-visible { outline: 3px solid #38bdf8; outline-offset: 3px; border-radius: 4px; }
    .skip-link { position: absolute; left: 1rem; top: -5rem; z-index: 3; background: #e2e8f0; color: #020617; padding: .65rem 1rem; border-radius: .5rem; }
    .skip-link:focus { top: 1rem; }
    .shell { width: min(100% - 2rem, 1240px); margin: 0 auto; padding: 2.25rem 0 3rem; }
    .topline { display: flex; align-items: flex-start; justify-content: space-between; gap: 1.5rem; margin-bottom: 1.6rem; }
    .eyebrow { margin: 0 0 .35rem; color: #38bdf8; font-size: .74rem; font-weight: 800; letter-spacing: .14em; text-transform: uppercase; }
    h1 { margin: 0; font-size: clamp(1.8rem, 4vw, 2.7rem); letter-spacing: -.04em; line-height: 1.1; overflow-wrap: anywhere; }
    h2, h3, p { margin-top: 0; }
    .lede { margin: .65rem 0 0; color: #94a3b8; }
    .refresh { white-space: nowrap; color: #94a3b8; font-size: .85rem; }
    .refresh-actions, .detail-status { display: flex; align-items: center; gap: .7rem; }
    .refresh-toggle { padding: .55rem .8rem; border: 1px solid #334155; border-radius: .6rem; background: #0f172a; color: #e2e8f0; cursor: pointer; font: inherit; font-size: .85rem; font-weight: 700; }
    .refresh-toggle:hover { border-color: #38bdf8; color: #bae6fd; }
    .refresh-toggle:focus-visible { outline: 3px solid #38bdf8; outline-offset: 3px; }
    .panel, .check-card, .event { border: 1px solid #1e293b; border-radius: 1rem; background: #0f172a; }
    .panel { padding: 1.25rem; }
    .summary-grid { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: .75rem; margin: 1.5rem 0; }
    .metric { padding: .9rem 1rem; border: 1px solid #1e293b; border-radius: .8rem; background: #0b1222; }
    .metric-label { display: block; color: #94a3b8; font-size: .75rem; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
    .metric-value { display: block; margin-top: .15rem; font-size: 1.55rem; font-weight: 750; font-variant-numeric: tabular-nums; }
    .notice { margin: 1rem 0; padding: .9rem 1rem; border: 1px solid #334155; border-radius: .75rem; background: #0b1222; color: #cbd5e1; }
    .notice strong { color: #e2e8f0; }
    .notice.error { border-color: #7f1d1d; background: #1c1119; color: #fecaca; }
    .section-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; margin: 2rem 0 .9rem; }
    .section-heading h2 { margin: 0; font-size: 1.15rem; }
    .muted { color: #94a3b8; }
    .check-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .9rem; }
    .check-card { min-width: 0; padding: 1.1rem; box-shadow: 0 12px 28px rgb(0 0 0 / .12); }
    .card-top { display: flex; align-items: flex-start; justify-content: space-between; gap: .8rem; }
    .check-name { margin: 0; font-size: 1.05rem; line-height: 1.3; overflow-wrap: anywhere; }
    .check-link { color: #e2e8f0; }
    .check-link:hover { color: #7dd3fc; }
    .badge { display: inline-flex; align-items: center; gap: .42rem; flex: 0 0 auto; padding: .25rem .58rem; border: 1px solid currentColor; border-radius: 999px; font-size: .72rem; font-weight: 800; letter-spacing: .04em; text-transform: uppercase; }
    .badge::before { width: .42rem; height: .42rem; border-radius: 50%; background: currentColor; content: ""; }
    .status-up { color: #4ade80; }
    .status-down { color: #fda4af; }
    .status-grace { color: #fbbf24; }
    .status-awaiting { color: #7dd3fc; }
    .status-paused, .status-unknown { color: #cbd5e1; }
    .meta-row { display: flex; flex-wrap: wrap; gap: .45rem; margin-top: .8rem; }
    .tag { padding: .16rem .48rem; border: 1px solid #334155; border-radius: 999px; color: #cbd5e1; font-size: .74rem; overflow-wrap: anywhere; }
    .facts { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: .8rem; margin: 1rem 0 0; }
    .facts div { min-width: 0; }
    .facts dt { color: #94a3b8; font-size: .72rem; font-weight: 700; letter-spacing: .06em; text-transform: uppercase; }
    .facts dd { margin: .2rem 0 0; color: #e2e8f0; font-size: .88rem; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
    .reason { margin: .9rem 0 0; color: #cbd5e1; font-size: .85rem; overflow-wrap: anywhere; }
    .back { display: inline-flex; margin: 0 0 1.25rem; font-weight: 700; }
    .detail-panel { margin-top: 1rem; }
    .detail-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; }
    .detail-header h2 { margin: 0; font-size: 1.2rem; }
    .detail-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .85rem; margin-top: 1.2rem; }
    .detail-fact { min-width: 0; padding: .85rem; border: 1px solid #1e293b; border-radius: .7rem; background: #0b1222; }
    .detail-fact strong { display: block; color: #94a3b8; font-size: .72rem; letter-spacing: .06em; text-transform: uppercase; }
    .detail-fact span { display: block; margin-top: .25rem; overflow-wrap: anywhere; }
    .event-list { display: grid; gap: .7rem; }
    .event { min-width: 0; padding: .9rem 1rem; }
    .event-top { display: flex; flex-wrap: wrap; align-items: center; gap: .6rem 1rem; }
    .event-kind { color: #e2e8f0; font-weight: 750; text-transform: capitalize; }
    .event-time, .event-meta { color: #94a3b8; font-size: .82rem; font-variant-numeric: tabular-nums; }
    .event-meta { display: flex; flex-wrap: wrap; gap: .4rem 1rem; margin-top: .55rem; }
    details { margin-top: .7rem; }
    summary { width: fit-content; color: #7dd3fc; cursor: pointer; font-size: .85rem; font-weight: 700; }
    pre { max-height: 24rem; overflow: auto; margin: .65rem 0 0; padding: .8rem; border: 1px solid #1e293b; border-radius: .6rem; background: #020617; color: #cbd5e1; white-space: pre-wrap; overflow-wrap: anywhere; font: .82rem/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
    .empty { padding: 1.2rem; border: 1px dashed #334155; border-radius: .8rem; color: #94a3b8; }
    .footer { margin-top: 2rem; color: #94a3b8; font-size: .8rem; }
    @media (max-width: 900px) { .check-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .summary-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
    @media (max-width: 600px) { .shell { width: min(100% - 1.1rem, 1240px); padding-top: 1.35rem; } .topline { display: block; } .refresh-actions, .detail-status { align-items: flex-start; flex-direction: column; margin-top: .8rem; } .summary-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: .5rem; margin: 1rem 0; } .summary-grid .metric:last-child { grid-column: 1 / -1; } .metric { padding: .55rem .7rem; } .metric-label { font-size: .68rem; } .metric-value { font-size: 1.25rem; } .check-grid, .detail-grid { grid-template-columns: minmax(0, 1fr); } .facts { grid-template-columns: minmax(0, 1fr); } .section-heading { align-items: flex-start; flex-direction: column; } .panel { padding: 1rem; } }
    @media (prefers-reduced-motion: no-preference) { a, summary { transition: color .15s ease; } }
  </style>
</head>
<body>
  <a class="skip-link" href="#main">Skip to content</a>
  <div class="shell">
    {{if .HasCurrent}}
      <a class="back" href="/">← All checks</a>
      <header class="topline">
        <div><p class="eyebrow">Health check detail</p><h1>{{.Current.Name}}</h1><p class="lede">{{.Current.Tags}}</p></div>
        <div class="detail-status"><span class="badge status-{{statusClass .Current.Status}}">{{.Current.Status}}</span><button class="refresh-toggle" id="refresh-toggle" type="button" aria-pressed="false">Pause auto-refresh</button></div>
      </header>
      <main id="main">
        {{if .Info.ReloadError}}<div class="notice error" role="alert"><strong>Configuration reload issue:</strong> {{.Info.ReloadError}}</div>{{end}}
        <section class="panel detail-panel" aria-labelledby="state-heading">
          <div class="detail-header"><div><p class="eyebrow">Current state</p><h2 id="state-heading">{{.Current.Status}}</h2></div></div>
          {{if .Current.Reason}}<p class="reason">{{.Current.Reason}}</p>{{end}}
          <div class="detail-grid">
            <div class="detail-fact"><strong>Schedule</strong><span>{{.Current.Schedule}}</span></div>
            <div class="detail-fact"><strong>Region</strong><span>{{region .Current.Region}}</span></div>
            <div class="detail-fact"><strong>Running</strong><span>{{if .Current.Started.IsZero}}No{{else}}Yes · started {{formatTime .Current.Started}}{{end}}</span></div>
            <div class="detail-fact"><strong>Last success</strong><span>{{formatTime .Current.LastSuccess}}</span></div>
            <div class="detail-fact"><strong>Last ping</strong><span>{{formatTime .Current.LastPing}}</span></div>
            <div class="detail-fact"><strong>Next due</strong><span>{{formatTime .Current.NextDue}}</span></div>
            <div class="detail-fact"><strong>Deadline</strong><span>{{formatTime .Current.Deadline}}</span></div>
            <div class="detail-fact"><strong>Last run duration</strong><span>{{formatDuration .Current.Duration}}</span></div>
            <div class="detail-fact"><strong>Depends on regions</strong><span>{{if .Current.DependsOnRegions}}{{range $i, $region := .Current.DependsOnRegions}}{{if $i}}, {{end}}{{$region}}{{end}}{{else}}None{{end}}</span></div>
          </div>
          {{if .Current.DeliveryError}}<div class="notice error" role="status"><strong>Delivery to {{if .Info.WebhookName}}{{.Info.WebhookName}}{{else}}webhook{{end}} failed:</strong> {{.Current.DeliveryError}}</div>{{else if .Current.Notify}}<p class="notice">Notifications use {{if .Info.WebhookName}}{{.Info.WebhookName}}{{else}}the configured webhook{{end}}.</p>{{end}}
        </section>
        <section aria-labelledby="events-heading">
          <div class="section-heading"><h2 id="events-heading">Recent events</h2><span class="muted">Newest first · up to 20 events</span></div>
          {{if .Current.Events}}
            <div class="event-list">
              {{range .Current.Events}}
                <article class="event">
                  <div class="event-top"><span class="event-kind">{{.Kind}}</span><time class="event-time" datetime="{{timeAttr .Time}}">{{formatTime .Time}}</time></div>
                  <div class="event-meta">
                    {{if .RID}}<span>RID: {{.RID}}</span>{{end}}
                    {{if .ExitCode}}<span>Exit code: {{.ExitCode}}</span>{{end}}
                    {{if .Duration}}<span>Duration: {{formatDuration .Duration}}</span>{{end}}
                    {{if .Truncated}}<span>Log was truncated</span>{{end}}
                  </div>
                  {{if .Body}}<details><summary>Log details</summary><pre>{{.Body}}</pre></details>{{end}}
                </article>
              {{end}}
            </div>
          {{else}}<p class="empty">No events have been received for this check.</p>{{end}}
        </section>
        <p class="footer">Auto-refresh runs every 15 seconds by default. Pause it while reading logs. State is held in this process and resets when it restarts.</p>
      </main>
    {{else}}
      <header class="topline">
        <div><p class="eyebrow">Health checks</p><h1>healthchecks-go</h1><p class="lede">A live view of your configured check-ins.</p></div>
        <div class="refresh-actions"><button class="refresh-toggle" id="refresh-toggle" type="button" aria-pressed="false">Pause auto-refresh</button><span class="refresh">Auto-refresh every 15 seconds</span></div>
      </header>
      <main id="main">
        <section class="summary-grid" aria-label="Check totals">
          <div class="metric"><span class="metric-label">Checks</span><span class="metric-value">{{.Total}}</span></div>
          <div class="metric"><span class="metric-label">Up</span><span class="metric-value status-up">{{.Up}}</span></div>
          <div class="metric"><span class="metric-label">Down</span><span class="metric-value status-down">{{.Down}}</span></div>
          <div class="metric"><span class="metric-label">In grace</span><span class="metric-value status-grace">{{.Grace}}</span></div>
          <div class="metric"><span class="metric-label">Awaiting / paused</span><span class="metric-value">{{.Awaiting}} / {{.Paused}}</span></div>
        </section>
        <div class="notice"><strong>In-memory, process-local state.</strong> This history and timing reset when the server process restarts. Process started {{formatTime .Info.Started}}.</div>
        {{if .Info.ReloadError}}<div class="notice error" role="alert"><strong>Configuration reload issue:</strong> {{.Info.ReloadError}}</div>{{end}}
        <div class="section-heading"><h2>Configured checks</h2><span class="muted">Down checks appear first</span></div>
        {{if .Checks}}
          <section class="check-grid" aria-label="Configured checks">
            {{range .Checks}}
              <article class="check-card status-{{statusClass .Status}}" data-status="{{.Status}}">
                <div class="card-top"><h3 class="check-name"><a class="check-link" href="/checks/{{.Key}}">{{.Name}}</a></h3><span class="badge status-{{statusClass .Status}}">{{.Status}}</span></div>
                <div class="meta-row"><span class="tag">Region: {{region .Region}}</span>{{if .Tags}}<span class="tag">{{.Tags}}</span>{{end}}</div>
                <dl class="facts">
                  <div><dt>Last success</dt><dd>{{formatTime .LastSuccess}}</dd></div>
                  <div><dt>Next due</dt><dd>{{formatTime .NextDue}}</dd></div>
                  <div><dt>Running</dt><dd>{{if .Started.IsZero}}No{{else}}Yes · {{formatTime .Started}}{{end}}</dd></div>
                  <div><dt>Schedule</dt><dd>{{.Schedule}}</dd></div>
                </dl>
                {{if .Reason}}<p class="reason">{{.Reason}}</p>{{end}}
              </article>
            {{end}}
          </section>
        {{else}}<p class="empty">No checks are configured.</p>{{end}}
        <p class="footer">Auto-refresh runs every 15 seconds by default. State is held in this process and resets when it restarts.</p>
      </main>
    {{end}}
  </div>
  <script>{{.Script}}</script>
</body>
</html>`

const dashboardRefreshScript = `(() => {
  const button = document.getElementById("refresh-toggle");
  if (!button) return;
  let timer = window.setTimeout(() => window.location.reload(), 15000);
  button.addEventListener("click", () => {
    if (timer !== null) {
      window.clearTimeout(timer);
      timer = null;
      button.textContent = "Resume auto-refresh";
      button.setAttribute("aria-pressed", "true");
      return;
    }
    timer = window.setTimeout(() => window.location.reload(), 15000);
    button.textContent = "Pause auto-refresh";
    button.setAttribute("aria-pressed", "false");
  });
})();`

const dashboardNotFound = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Not found · healthchecks-go</title></head><body style="margin:0;background:#020617;color:#e2e8f0;font:16px system-ui,sans-serif"><main style="max-width:40rem;margin:12vh auto;padding:2rem"><p style="color:#38bdf8;font-weight:700;text-transform:uppercase;letter-spacing:.12em">healthchecks-go</p><h1>Check not found</h1><p style="color:#94a3b8">This page does not match a configured check.</p><a href="/" style="color:#7dd3fc">Return to all checks</a></main></body></html>`

func renderDashboard(w http.ResponseWriter, r *http.Request, e *Engine) {
	setDashboardHeaders(w)
	if r.URL.EscapedPath() != r.URL.Path || (r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/checks/")) {
		writeDashboardError(w, r, http.StatusNotFound, dashboardNotFound)
		return
	}

	views := e.Snapshot()
	data := dashboardData{Info: e.Info(), Checks: views, Total: len(views), Script: template.JS(dashboardRefreshScript)}
	if r.URL.Path != "/" {
		key := strings.TrimPrefix(r.URL.Path, "/checks/")
		if key == "" || strings.Contains(key, "/") {
			writeDashboardError(w, r, http.StatusNotFound, dashboardNotFound)
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
			writeDashboardError(w, r, http.StatusNotFound, dashboardNotFound)
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

	var body bytes.Buffer
	if err := dashboardTemplate.Execute(&body, data); err != nil {
		writeDashboardError(w, r, http.StatusInternalServerError, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Dashboard unavailable</title></head><body><h1>Dashboard unavailable</h1></body></html>`)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body.Bytes())
	}
}

func setDashboardHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	refreshScriptHash := sha256.Sum256([]byte(dashboardRefreshScript))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(refreshScriptHash[:])+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
