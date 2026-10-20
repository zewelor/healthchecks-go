package healthchecks

import (
	"bytes"
	"crypto/sha256"
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
	Script           template.JS
	ThemeScript      template.JS
	Now              time.Time
}

type dashboardTimestamp struct {
	Value, Now time.Time
	Due        bool
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
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

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{if .HasCurrent}}{{.Current.Name}} · {{end}}{{.Info.SiteName}}</title>
  <script>{{.ThemeScript}}</script>
  <style>
    :root { color-scheme: light; --bg: #f8fafc; --text: #0f172a; --link: #0369a1; --link-hover: #075985; --accent: #475569; --muted: #475569; --border-strong: #cbd5e1; --surface: #fff; --border: #e2e8f0; --inset: #f1f5f9; --secondary: #334155; --error-border: #fda4af; --error-bg: #fff1f2; --error-text: #9f1239; --up: #15803d; --down: #be123c; --grace: #92400e; --control-bg: #f3f3f3; --control-text: #151515; }
    :root[data-theme="dark"] { color-scheme: dark; --bg: #020617; --text: #e2e8f0; --link: #7dd3fc; --link-hover: #bae6fd; --accent: #94a3b8; --muted: #94a3b8; --border-strong: #334155; --surface: #0f172a; --border: #1e293b; --inset: #0b1222; --secondary: #cbd5e1; --error-border: #7f1d1d; --error-bg: #1c1119; --error-text: #fecaca; --up: #4ade80; --down: #fda4af; --grace: #fbbf24; --control-bg: #151b27; --control-text: #e5e7eb; }
    @media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { color-scheme: dark; --bg: #020617; --text: #e2e8f0; --link: #7dd3fc; --link-hover: #bae6fd; --accent: #94a3b8; --muted: #94a3b8; --border-strong: #334155; --surface: #0f172a; --border: #1e293b; --inset: #0b1222; --secondary: #cbd5e1; --error-border: #7f1d1d; --error-bg: #1c1119; --error-text: #fecaca; --up: #4ade80; --down: #fda4af; --grace: #fbbf24; --control-bg: #151b27; --control-text: #e5e7eb; } }

    :root { font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: var(--bg); color: var(--text); font-synthesis: none; }
    * { box-sizing: border-box; }
    body { margin: 0; min-width: 320px; background: var(--bg); color: var(--text); line-height: 1.5; }
    a { color: var(--text); text-decoration: none; }
    a:hover { color: var(--text); text-decoration: underline; }
    a:focus-visible, summary:focus-visible { outline: 3px solid var(--accent); outline-offset: 3px; border-radius: 4px; }
    .skip-link { position: absolute; left: 1rem; top: -5rem; z-index: 3; background: var(--text); color: var(--bg); padding: .65rem 1rem; border-radius: .5rem; }
    .skip-link:focus { top: 1rem; }
    .shell { width: min(100% - 2rem, 1240px); margin: 0 auto; padding: 2.25rem 0 3rem; }
    .topline > div:first-child { min-width: 0; }
    .topline { display: flex; align-items: flex-start; justify-content: space-between; gap: 1.5rem; margin-bottom: 1.6rem; }
    h1 { margin: 0; font-size: clamp(1.6rem, 4vw, 2rem); letter-spacing: -.04em; line-height: 1.1; overflow-wrap: anywhere; }
    h2, h3, p { margin-top: 0; }
    .lede { margin: .65rem 0 0; color: var(--muted); }
    .detail-status { display: flex; flex-wrap: wrap; justify-content: flex-end; align-items: center; gap: .7rem; }
    .panel, .check-card { border: 1px solid var(--border); border-radius: 1rem; background: var(--surface); }
    .panel { padding: 1.25rem; }
    .summary-bar { display: flex; align-items: center; gap: 1.25rem; margin: 1.75rem 0 1.25rem; padding-bottom: 1.25rem; border-bottom: 1px solid var(--border); }
    .summary-grid { display: grid; flex: 1; min-width: 0; grid-template-columns: repeat(6, minmax(0, 1fr)); gap: .5rem; }
    .metric, .toolbar { border-radius: .65rem; background: transparent; }
    .metric { padding: .65rem .8rem; border: 0; color: var(--text); font: inherit; text-align: left; cursor: pointer; }
    .metric:hover, .metric[aria-pressed="true"], .toolbar:hover, .toolbar:focus-within { background: var(--inset); }
    button:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }
    .metric-label { display: block; color: var(--muted); font-size: .68rem; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
    .metric-value { display: block; margin-top: .15rem; font-size: 1.3rem; font-weight: 750; font-variant-numeric: tabular-nums; }
    .notice { margin: 1rem 0; padding: .9rem 1rem; border: 1px solid var(--border-strong); border-radius: .75rem; background: var(--inset); color: var(--secondary); }
    .notice strong { color: var(--text); }
    .notice.error { border-color: var(--error-border); background: var(--error-bg); color: var(--error-text); }
    .section-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; margin: 2rem 0 .9rem; }
    .section-heading h2 { margin: 0; font-size: 1.15rem; }
    .muted { color: var(--muted); }
    .check-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .9rem; }
    .check-card { min-width: 0; padding: 1.1rem; }
    .card-top { display: flex; align-items: flex-start; justify-content: space-between; gap: .8rem; }
    .check-name { margin: 0; font-size: .95rem; line-height: 1.3; overflow-wrap: anywhere; }
    .check-link { color: var(--text); }
    .check-link:hover { color: var(--text); }
    .badge { display: inline-flex; align-items: center; gap: .42rem; flex: 0 0 auto; padding: .25rem .58rem; border: 1px solid currentColor; border-radius: 999px; font-size: .65rem; font-weight: 750; letter-spacing: .04em; text-transform: uppercase; }
    .badge::before { width: .42rem; height: .42rem; border-radius: 50%; background: currentColor; content: ""; }
    .status-up { color: var(--up); }
    .status-down { color: var(--down); }
    .status-grace { color: var(--grace); }
    .status-awaiting { color: var(--link); }
    .status-paused, .status-unknown { color: var(--secondary); }
    .meta-row { display: flex; flex-wrap: wrap; gap: .45rem; margin-top: .8rem; }
    .tag { padding: .16rem .48rem; border: 1px solid var(--border-strong); border-radius: 999px; color: var(--secondary); font-size: .74rem; overflow-wrap: anywhere; }
    .facts { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: .8rem; margin: 1rem 0 0; }
    .facts div { min-width: 0; }
    .facts dt { color: var(--muted); font-size: .72rem; font-weight: 700; letter-spacing: .06em; text-transform: uppercase; }
    .facts dd { margin: .2rem 0 0; color: var(--text); font-size: .88rem; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
    .reason { margin: .9rem 0 0; color: var(--secondary); font-size: .85rem; overflow-wrap: anywhere; }
    .back { display: inline-flex; margin: 0 0 1.25rem; font-weight: 700; }
    .detail-panel { margin-top: 1rem; }
    .detail-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 1.4rem 2rem; }
    .detail-fact { min-width: 0; }
    .dependencies { grid-column: 1 / -1; border-top: 1px solid var(--border); padding-top: 1rem; }
    .detail-fact strong { display: block; color: var(--muted); font-size: .72rem; letter-spacing: .06em; text-transform: uppercase; }
    .detail-fact span { display: block; margin-top: .25rem; overflow-wrap: anywhere; }
    .event-list { display: grid; gap: .7rem; }
    .event { min-width: 0; padding: .8rem 0; border-bottom: 1px solid var(--border); }
    .event-top { display: flex; flex-wrap: wrap; align-items: center; gap: .6rem 1rem; }
    .event-kind { color: var(--text); font-weight: 750; text-transform: capitalize; }
    .event-time, .event-meta { color: var(--muted); font-size: .82rem; font-variant-numeric: tabular-nums; }
    .event-meta { display: flex; flex-wrap: wrap; gap: .4rem 1rem; margin-top: .55rem; }
    details { margin-top: .7rem; }
    summary { width: fit-content; color: var(--text); cursor: pointer; font-size: .85rem; font-weight: 700; }
    pre { max-height: 24rem; overflow: auto; margin: .65rem 0 0; padding: .8rem; border: 1px solid var(--border); border-radius: .6rem; background: var(--bg); color: var(--secondary); white-space: pre-wrap; overflow-wrap: anywhere; font: .82rem/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
    .empty { padding: 1.2rem; border: 1px dashed var(--border-strong); border-radius: .8rem; color: var(--muted); }
    @media (max-width: 900px) { .check-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .summary-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
    @media (max-width: 600px) { .shell { width: min(100% - 1.1rem, 1240px); padding-top: 1.35rem; } .topline { gap: .75rem; flex-wrap: wrap; } .detail-status { align-items: flex-end; flex-direction: column; flex-shrink: 0; } .summary-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .5rem; } .summary-bar { gap: .75rem; margin: 1rem 0; } .metric { padding: .55rem .7rem; } .metric-label { font-size: .68rem; } .metric-value { font-size: 1.25rem; } .check-grid, .detail-grid { grid-template-columns: minmax(0, 1fr); } .section-heading { align-items: flex-start; flex-direction: column; } .panel { padding: 1rem; } }
    @media (prefers-reduced-motion: no-preference) { a, summary { transition: color .15s ease; } }
    select { padding: .55rem .5rem; border: 1px solid var(--border-strong); border-radius: .6rem; background: var(--surface); color: var(--text); font: inherit; }
    select option { background: var(--surface); color: var(--text); }
    select:focus-visible, time:focus-visible { outline: 3px solid var(--accent); outline-offset: 3px; }
    time[data-full] { position: relative; cursor: help; }
    time[data-full]:hover::after, time[data-full]:focus::after { content: attr(data-full); position: absolute; z-index: 2; bottom: calc(100% + .4rem); left: 0; box-sizing: border-box; width: max-content; max-width: min(18rem, var(--tooltip-max-width, calc(100vw - 2rem))); padding: .4rem .6rem; border: 1px solid var(--border-strong); border-radius: .4rem; background: var(--surface); color: var(--text); box-shadow: 0 4px 12px rgb(0 0 0 / .15); font-size: .78rem; pointer-events: none; }
    time[data-positioned]:hover::after, time[data-positioned]:focus::after { position: fixed; top: var(--tooltip-top); left: var(--tooltip-left); bottom: auto; }
    .toolbar { position: relative; display: flex; flex-direction: column; align-items: flex-start; align-self: stretch; gap: .15rem; flex-shrink: 0; padding: .65rem .8rem; }
    .sort-control { display: flex; align-items: center; height: 1.02rem; color: var(--muted); }
    .sort-control svg, .sort-chevron { width: 16px; height: 16px; fill: none; stroke: currentColor; stroke-width: 1.5; stroke-linecap: round; stroke-linejoin: round; }
    .toolbar select { height: 1.95rem; padding: 0 1.25rem 0 0; border: 0; border-radius: .3rem; appearance: none; background: transparent; color: var(--text); font-size: 1.05rem; font-weight: 650; line-height: 1.5; cursor: pointer; }
    .sort-chevron { position: absolute; right: .8rem; bottom: 1.25rem; width: 12px; height: 12px; color: var(--muted); pointer-events: none; }
    @media (max-width: 600px) { .toolbar { width: 34px; height: 34px; align-self: center; align-items: center; justify-content: center; padding: 0; } .toolbar select { position: absolute; inset: 0; width: 100%; height: 100%; opacity: 0; } .toolbar .sort-chevron { display: none; } .toolbar:focus-within { outline: 2px solid var(--accent); outline-offset: 3px; } }
    .theme-switch { position: relative; display: inline-flex; gap: 2px; padding: 3px; border: 0; border-radius: 999px; background: var(--control-bg); }
    .theme-switch::before { content: ""; position: absolute; top: 3px; left: 3px; width: 28px; height: 28px; border-radius: 50%; background: var(--control-text); transform: translateX(0); }
    .theme-switch[data-choice="light"]::before { transform: translateX(30px); }
    .theme-switch[data-choice="dark"]::before { transform: translateX(60px); }
    .theme-switch button { position: relative; display: grid; place-items: center; width: 28px; height: 28px; padding: 6px; border: 0; border-radius: 50%; background: transparent; color: var(--muted); cursor: pointer; }
    .theme-switch button:hover { color: var(--control-text); }
    .theme-switch button[aria-pressed="true"] { color: var(--control-bg); }
    .theme-switch svg { width: 16px; height: 16px; fill: none; stroke: currentColor; stroke-width: 1.5; stroke-linecap: round; stroke-linejoin: round; }
    @media (prefers-reduced-motion: no-preference) { .theme-switch::before { transition: transform .18s ease; } }
  </style>
</head>
<body>
  <a class="skip-link" href="#main">Skip to content</a>
  <div class="shell">
    {{if .HasCurrent}}
      <a class="back" href="/">← All checks</a>
      <header class="topline">
        <div><h1>{{.Current.Name}}</h1>{{if .Current.Tags}}<p class="lede">{{.Current.Tags}}</p>{{end}}</div>
        <div class="detail-status">{{template "themeControl"}}<span class="badge status-{{statusClass .Current.Status}}">{{.Current.Status}}</span></div>
      </header>
      <main id="main">
        {{if .Info.StorageError}}<div class="notice error" role="alert">{{.Info.StorageError}}</div>{{end}}
        {{if .Info.ReloadError}}<div class="notice error" role="alert"><strong>Configuration reload issue:</strong> {{.Info.ReloadError}}</div>{{end}}
        <section class="panel detail-panel" aria-label="Check details">
          {{if .Current.Reason}}<p class="reason">{{.Current.Reason}}</p>{{end}}
          <div class="detail-grid">
            <div class="detail-fact"><strong>Schedule</strong><span>{{.Current.Schedule}}</span></div>
            <div class="detail-fact"><strong>Running</strong><span>{{if .Current.Started.IsZero}}No{{else}}Yes · started {{template "timestamp" (timestamp .Current.Started $.Now)}}{{end}}</span></div>
            <div class="detail-fact"><strong>Last success</strong><span>{{template "timestamp" (timestamp .Current.LastSuccess $.Now)}}</span></div>
            <div class="detail-fact"><strong>Last ping</strong><span>{{template "timestamp" (timestamp .Current.LastPing $.Now)}}</span></div>
            <div class="detail-fact"><strong>Next due</strong><span>{{template "timestamp" (dueTimestamp .Current.NextDue $.Now .Current.Status)}}</span></div>
            <div class="detail-fact"><strong>Deadline</strong><span>{{template "timestamp" (dueTimestamp .Current.Deadline $.Now .Current.Status)}}</span></div>
            {{if .Current.Duration}}<div class="detail-fact"><strong>Last run duration</strong><span>{{formatDuration .Current.Duration}}</span></div>{{end}}
            {{if .Current.DependsOnRegions}}<div class="detail-fact dependencies"><strong>Depends on regions</strong><span>{{range $i, $region := .Current.DependsOnRegions}}{{if $i}}, {{end}}{{$region}}{{end}}</span></div>{{end}}
          </div>
          {{if .Current.DeliveryError}}<div class="notice error" role="status"><strong>Delivery to {{if .Info.WebhookName}}{{.Info.WebhookName}}{{else}}webhook{{end}} failed:</strong> {{.Current.DeliveryError}}</div>{{else if .Current.Notify}}<p class="notice">Notifications use {{if .Info.WebhookName}}{{.Info.WebhookName}}{{else}}the configured webhook{{end}}.</p>{{end}}
        </section>
        <section aria-labelledby="events-heading">
          <div class="section-heading"><h2 id="events-heading">Recent events</h2></div>
          {{if .Current.Events}}
            <div class="event-list">
              {{range .Current.Events}}
                <article class="event">
                  <div class="event-top"><span class="event-kind">{{.Kind}}</span><span class="event-time">{{template "timestamp" (timestamp .Time $.Now)}}</span></div>
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

      </main>
    {{else}}
      <header class="topline">
        <div><h1>{{.Info.SiteName}}</h1></div>
        {{template "themeControl"}}
      </header>
      <main id="main">
        <div class="summary-bar">
        <section class="summary-grid" aria-label="Filter checks by status">
          <button class="metric" type="button" data-filter="all" aria-pressed="true"><span class="metric-label">Checks</span><span class="metric-value">{{.Total}}</span></button>
          <button class="metric" type="button" data-filter="up" aria-pressed="false"><span class="metric-label">Up</span><span class="metric-value status-up">{{.Up}}</span></button>
          <button class="metric" type="button" data-filter="down" aria-pressed="false"><span class="metric-label">Down</span><span class="metric-value status-down">{{.Down}}</span></button>
          <button class="metric" type="button" data-filter="grace" aria-pressed="false"><span class="metric-label">In grace</span><span class="metric-value status-grace">{{.Grace}}</span></button>
          <button class="metric" type="button" data-filter="awaiting" aria-pressed="false"><span class="metric-label">Awaiting</span><span class="metric-value">{{.Awaiting}}</span></button>
          <button class="metric" type="button" data-filter="paused" aria-pressed="false"><span class="metric-label">Paused</span><span class="metric-value">{{.Paused}}</span></button>
        </section>
        <div class="toolbar"><span class="sort-control" title="Sort by" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M8 4v16m-4-4 4 4 4-4M16 20V4m-4 4 4-4 4 4"/></svg></span><select id="sort-select" aria-label="Sort by"><option value="state">State</option><option value="name">Name</option><option value="due">Next due</option></select><svg class="sort-chevron" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg></div>
        </div>
        {{if .Info.StorageError}}<div class="notice error" role="alert">{{.Info.StorageError}}</div>{{end}}
        {{if .Info.ReloadError}}<div class="notice error" role="alert"><strong>Configuration reload issue:</strong> {{.Info.ReloadError}}</div>{{end}}
        {{if .Checks}}
          <section class="check-grid" aria-label="Configured checks">
            {{range .Checks}}
              <article class="check-card status-{{statusClass .Status}}" data-status="{{.Status}}" data-name="{{.Name}}" data-due="{{if ne .Status "paused"}}{{timeAttr .NextDue}}{{end}}">
                <div class="card-top"><h2 class="check-name"><a class="check-link" href="/checks/{{.Key}}">{{.Name}}</a></h2><span class="badge status-{{statusClass .Status}}">{{.Status}}</span></div>
                {{if .Tags}}<div class="meta-row"><span class="tag">{{.Tags}}</span></div>{{end}}
                <dl class="facts">
                  <div><dt>Last success</dt><dd>{{template "timestamp" (timestamp .LastSuccess $.Now)}}</dd></div>
                  <div><dt>Next due</dt><dd>{{template "timestamp" (dueTimestamp .NextDue $.Now .Status)}}</dd></div>
                </dl>
                {{if not .Started.IsZero}}<p class="reason">Running since {{template "timestamp" (timestamp .Started $.Now)}}</p>{{end}}
                {{if .Reason}}<p class="reason">{{.Reason}}</p>{{end}}
              </article>
            {{end}}
          </section>
          <p id="filter-empty" class="empty" role="status" hidden>No checks match this status.</p>
        {{else}}<p class="empty">No checks are configured.</p>{{end}}

      </main>
    {{end}}
  </div>
  <script>{{.Script}}</script>
</body>
</html>
{{define "timestamp"}}{{if .Value.IsZero}}—{{else}}<time tabindex="0" datetime="{{timeAttr .Value}}" data-full="{{formatTime .Value}}" aria-label="{{formatTime .Value}}"{{if .Due}} data-due="true"{{end}}>{{if .Due}}{{dueTime .Value .Now}}{{else}}{{relativeTime .Value .Now}}{{end}}</time>{{end}}{{end}}
{{define "themeControl"}}<div class="theme-switch" role="group" aria-label="Theme" data-choice="system">
  <button type="button" data-theme-choice="system" aria-label="System theme" title="System theme" aria-pressed="true"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8M12 17v4"/></svg></button>
  <button type="button" data-theme-choice="light" aria-label="Light theme" title="Light theme" aria-pressed="false"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5"/></svg></button>
  <button type="button" data-theme-choice="dark" aria-label="Dark theme" title="Dark theme" aria-pressed="false"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20.5 14A9 9 0 0 1 10 3.5 9 9 0 1 0 20.5 14Z"/></svg></button>
</div>{{end}}`

const dashboardThemeScript = `(() => {
  try {
    const theme = localStorage.getItem("hc-theme");
    if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme;
  } catch (error) {
    // Storage can be disabled by the browser; system colors still work.
    console.warn("Theme preference could not be read.", error);
  }
})();`

const dashboardRefreshScript = `(() => {
  const savePreference = (key, value) => {
    try { localStorage.setItem(key, value); }
    catch (error) { console.warn("Preference could not be saved.", error); }
  };
  const readPreference = key => {
    try { return localStorage.getItem(key); }
    catch (error) { console.warn("Preference could not be read.", error); return null; }
  };
  const themeSwitch = document.querySelector(".theme-switch");
  const themeButtons = themeSwitch.querySelectorAll("button");
  const selectTheme = theme => {
    themeSwitch.dataset.choice = theme;
    themeButtons.forEach(button => button.setAttribute("aria-pressed", String(button.dataset.themeChoice === theme)));
    if (theme === "system") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
  };
  selectTheme(document.documentElement.dataset.theme || "system");
  themeButtons.forEach(button => button.addEventListener("click", () => {
    selectTheme(button.dataset.themeChoice);
    savePreference("hc-theme", button.dataset.themeChoice);
  }));
  const grid = document.querySelector(".check-grid");
  if (grid) {
    const cards = Array.from(grid.querySelectorAll(".check-card"));
    const sort = document.getElementById("sort-select");
    const filters = Array.from(document.querySelectorAll("[data-filter]"));
    const savedSort = readPreference("hc-sort");
    if (["state", "name", "due"].includes(savedSort)) sort.value = savedSort;
    let status = readPreference("hc-status") || "all";
    if (!filters.some(button => button.dataset.filter === status)) status = "all";
    const ranks = { down: 0, grace: 1, awaiting: 2, up: 3, paused: 4 };
    const names = new Intl.Collator("en", { numeric: true, sensitivity: "base" });
    const updateChecks = () => {
      cards.sort((a, b) => {
        let order = 0;
        if (sort.value === "state") order = ranks[a.dataset.status] - ranks[b.dataset.status];
        if (sort.value === "due") order = (Date.parse(a.dataset.due) || Infinity) - (Date.parse(b.dataset.due) || Infinity);
        return order || names.compare(a.dataset.name, b.dataset.name);
      });
      cards.forEach(card => { card.hidden = status !== "all" && card.dataset.status !== status; grid.append(card); });
      filters.forEach(button => button.setAttribute("aria-pressed", String(button.dataset.filter === status)));
      document.getElementById("filter-empty").hidden = cards.some(card => !card.hidden);
    };
    sort.addEventListener("change", () => { savePreference("hc-sort", sort.value); updateChecks(); });
    filters.forEach(button => button.addEventListener("click", () => {
      status = button.dataset.filter;
      savePreference("hc-status", status);
      updateChecks();
    }));
    updateChecks();
  }
  const formatter = new Intl.RelativeTimeFormat("en", { numeric: "always" });
  const fullTimeFormatter = new Intl.DateTimeFormat(undefined, {
    year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit", timeZoneName: "short"
  });
  const updateTimes = () => {
    document.querySelectorAll("time[data-full]").forEach(element => {
      const date = new Date(element.dateTime);
      const fullTime = fullTimeFormatter.format(date);
      element.dataset.full = fullTime;
      element.setAttribute("aria-label", fullTime);
      const seconds = (date.getTime() - Date.now()) / 1000;
      const [size, unit] = Math.abs(seconds) >= 84600 ? [86400, "day"] : Math.abs(seconds) >= 3570 ? [3600, "hour"] : Math.abs(seconds) >= 60 ? [60, "minute"] : [1, "second"];
      const amount = Math.sign(seconds) * Math.round(Math.abs(seconds) / size);
      const text = amount === 0 ? "now" : formatter.format(amount, unit);
      element.textContent = element.dataset.due === "true" ? text.replace(/ ago$/, " overdue") : text;
    });
  };
  updateTimes();
  const positionTooltip = element => {
    const rect = element.getBoundingClientRect();
    const viewport = document.documentElement;
    element.style.setProperty("--tooltip-max-width", viewport.clientWidth - 32 + "px");
    const style = getComputedStyle(element, "::after");
    const width = parseFloat(style.width);
    const height = parseFloat(style.height);
    const left = Math.max(16, Math.min(rect.left, viewport.clientWidth - width - 16));
    const above = rect.top - height - 6;
    const top = Math.max(16, Math.min(above >= 16 ? above : rect.bottom + 6, viewport.clientHeight - height - 16));
    element.style.setProperty("--tooltip-left", left + "px");
    element.style.setProperty("--tooltip-top", top + "px");
    element.dataset.positioned = "true";
  };
  document.querySelectorAll("time[data-full]").forEach(element => {
    element.addEventListener("mouseenter", () => positionTooltip(element));
    element.addEventListener("focus", () => positionTooltip(element));
  });
  const positionVisibleTooltips = () => document.querySelectorAll("time[data-full]:hover, time[data-full]:focus").forEach(positionTooltip);
  window.addEventListener("resize", positionVisibleTooltips);
  window.addEventListener("scroll", positionVisibleTooltips, true);
  window.setInterval(updateTimes, 1000);
  window.setTimeout(() => window.location.reload(), 15000);
})();`

const dashboardNotFound = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Check not found</title></head><body style="margin:0;background:#020617;color:#e2e8f0;font:16px system-ui,sans-serif"><main style="max-width:40rem;margin:12vh auto;padding:2rem"><h1>Check not found</h1><p style="color:#94a3b8">This page does not match a configured check.</p><a href="/" style="color:#7dd3fc">Return to all checks</a></main></body></html>`

func renderDashboard(w http.ResponseWriter, r *http.Request, e *Engine) {
	setDashboardHeaders(w)
	if r.URL.EscapedPath() != r.URL.Path || (r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/checks/")) {
		writeDashboardError(w, r, http.StatusNotFound, dashboardNotFound)
		return
	}

	views := e.Snapshot()
	data := dashboardData{Info: e.Info(), Checks: views, Total: len(views), Script: template.JS(dashboardRefreshScript), ThemeScript: template.JS(dashboardThemeScript), Now: e.now()}
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
	themeScriptHash := sha256.Sum256([]byte(dashboardThemeScript))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(refreshScriptHash[:])+"' 'sha256-"+base64.StdEncoding.EncodeToString(themeScriptHash[:])+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
