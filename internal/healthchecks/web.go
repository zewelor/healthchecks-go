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
	NotFound         bool
	Script           template.JS
	ThemeScript      template.JS
	Now              time.Time
}

type dashboardTimestamp struct {
	Value, Now time.Time
	Due        bool
}

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

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{if .NotFound}}Check not found · {{else if .HasCurrent}}{{.Current.Name}} · {{end}}{{.Info.SiteName}}</title>
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
    #refresh-error { position: fixed; bottom: 1rem; left: 50%; transform: translateX(-50%); z-index: 4; width: min(40rem, calc(100% - 2rem)); margin: 0; }
    .section-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; margin: 2rem 0 .9rem; }
    .section-heading h2 { margin: 0; font-size: 1.15rem; }
    .section-heading p { margin: 0; font-size: .8rem; }
    .muted { color: var(--muted); }
    .check-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .9rem; }
    .check-card { min-width: 0; padding: 1.1rem; }
    .card-top { display: flex; align-items: flex-start; justify-content: space-between; gap: .8rem; }
    .check-name { margin: 0; font-size: .95rem; line-height: 1.3; overflow-wrap: anywhere; }
    .check-link { color: var(--text); }
    .check-link:hover { color: var(--text); }
    .description-tooltip, [data-full]::after { visibility: hidden; z-index: 3; box-sizing: border-box; width: max-content; padding: .65rem .85rem; border: 1px solid var(--border-strong); border-radius: .6rem; background: var(--border); color: var(--secondary); box-shadow: 0 6px 20px rgb(0 0 0 / .12); font-size: .85rem; font-weight: 400; line-height: 1.5; white-space: pre-wrap; overflow-wrap: anywhere; transition: visibility 0s; }
    .description-tooltip { position: fixed; top: var(--tooltip-top); left: var(--tooltip-left); max-width: min(24rem, calc(100vw - 2rem)); max-height: 50vh; overflow: auto; transition-delay: .12s; }
    .check-name:hover .description-tooltip:not([data-dismissed]), .check-name:focus-within .description-tooltip:not([data-dismissed]), [data-full]:hover:not([data-dismissed])::after, [data-full]:focus:not([data-dismissed])::after { visibility: visible; transition-delay: .3s; }
    .check-description { max-width: 70ch; margin: .8rem 0 0; color: var(--muted); font-size: .95rem; white-space: pre-wrap; overflow-wrap: anywhere; }
    .badge { display: inline-flex; align-items: center; gap: .42rem; flex: 0 0 auto; padding: .25rem .58rem; border: 1px solid currentColor; border-radius: 999px; font-size: .65rem; font-weight: 750; letter-spacing: .04em; text-transform: uppercase; }
    .badge::before { width: .42rem; height: .42rem; border-radius: 50%; background: currentColor; content: ""; }
    .status-up { color: var(--up); }
    .status-down { color: var(--down); }
    .status-grace { color: var(--grace); }
    .status-awaiting { color: var(--link); }
    .status-paused, .status-unknown { color: var(--secondary); }
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
    .event-list { margin: 0; padding: .25rem 1.25rem; list-style: none; }
    .event { color: var(--secondary); position: relative; min-width: 0; padding: 1rem 0 1rem 1.65rem; }
    .event:not(:last-child)::after { content: ""; position: absolute; left: 6px; top: calc(1.45rem + 13px); bottom: -1.45rem; width: 1px; background: var(--border); }
    .event::before { content: ""; position: absolute; box-sizing: border-box; left: 0; top: 1.45rem; width: 13px; height: 13px; border: 2px solid currentColor; border-radius: 50%; background: var(--surface); }
    .event.success::before { border-color: var(--up); background: var(--up); }
    .event.fail::before { border-color: var(--down); background: var(--down); }
    .event-top { display: flex; align-items: baseline; justify-content: space-between; gap: .5rem 1rem; }
    .event-top > div { display: flex; flex-wrap: wrap; align-items: baseline; gap: .25rem .75rem; }
    .event-top strong { color: var(--text); font-weight: 650; font-size: .9rem; }
    .event-top span, .event-top time, .event-meta { color: var(--muted); font-size: .8rem; font-variant-numeric: tabular-nums; }
    .event-top > time { flex-shrink: 0; text-align: right; }
    .event-meta { display: flex; flex-wrap: wrap; gap: .4rem 1rem; margin-top: .4rem; overflow-wrap: anywhere; }
    .event details { margin-top: .4rem; color: var(--text); }
    .event summary { font-size: .8rem; font-weight: 400; color: var(--muted); }
    @media (max-width: 600px) { .event { padding-left: 1.2rem; } .event-top { flex-wrap: wrap; } .event-top > time { text-align: left; } }
    details { margin-top: .7rem; }
    summary { width: fit-content; color: var(--text); cursor: pointer; font-size: .85rem; font-weight: 700; }
    pre { max-height: 24rem; overflow: auto; margin: .65rem 0 0; padding: .8rem; border: 1px solid var(--border); border-radius: .6rem; background: var(--bg); color: var(--secondary); white-space: pre-wrap; overflow-wrap: anywhere; font: .82rem/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
    .empty { padding: 1.2rem; border: 1px dashed var(--border-strong); border-radius: .8rem; color: var(--muted); }
    @media (max-width: 900px) { .check-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .summary-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
    @media (max-width: 600px) { .shell { width: min(100% - 1.1rem, 1240px); padding-top: 1.35rem; } .topline { gap: .75rem; flex-wrap: wrap; } .detail-status { width: 100%; align-items: center; justify-content: space-between; flex-shrink: 0; } .summary-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .5rem; } .summary-bar { gap: .75rem; margin: 1rem 0; } .metric { padding: .55rem .7rem; } .metric-label { font-size: .68rem; } .metric-value { font-size: 1.25rem; } .check-grid { grid-template-columns: minmax(0, 1fr); } .detail-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1rem; } .section-heading { align-items: flex-start; flex-direction: column; } .panel { padding: 1rem; } }
    @media (prefers-reduced-motion: no-preference) { a, summary { transition: color .15s ease; } }
    select { padding: .55rem .5rem; border: 1px solid var(--border-strong); border-radius: .6rem; background: var(--surface); color: var(--text); font: inherit; }
    select option { background: var(--surface); color: var(--text); }
    select:focus-visible, time:focus-visible { outline: 3px solid var(--accent); outline-offset: 3px; }
    time[data-full] { position: relative; cursor: help; }
    [data-full]::after { content: attr(data-full); position: absolute; bottom: calc(100% + .4rem); left: 0; max-width: min(18rem, var(--tooltip-max-width, calc(100vw - 2rem))); pointer-events: none; }
    [data-positioned]::after { position: fixed; top: var(--tooltip-top); left: var(--tooltip-left); bottom: auto; }
    .toolbar { position: relative; display: flex; flex-direction: column; align-items: flex-start; align-self: stretch; gap: .15rem; flex-shrink: 0; padding: .65rem .8rem; }
    .sort-control { display: grid; place-items: center; width: 32px; height: 28px; margin: -6px -8px; padding: 0; border: 0; border-radius: .35rem; background: transparent; color: var(--text); cursor: pointer; }
    .sort-control:hover { background: var(--border); }
    .sort-control .sort-down, .sort-control[data-direction="desc"] .sort-up { opacity: .3; }
    .sort-control[data-direction="desc"] .sort-down { opacity: 1; }
    .sort-control svg, .sort-chevron { width: 16px; height: 16px; fill: none; stroke: currentColor; stroke-width: 1.5; stroke-linecap: round; stroke-linejoin: round; }
    .toolbar select { height: 1.95rem; padding: 0 1.25rem 0 0; border: 0; border-radius: .3rem; appearance: none; background: transparent; color: var(--text); font-size: 1.05rem; font-weight: 650; line-height: 1.5; cursor: pointer; }
    .sort-chevron { position: absolute; right: .8rem; bottom: 1.25rem; width: 12px; height: 12px; color: var(--muted); pointer-events: none; }
    @media (max-width: 600px) { .toolbar { align-self: center; gap: 0; padding: 0; } .sort-control { width: 100%; height: 32px; margin: 0; } .toolbar select { height: 32px; padding: 0 .25rem; appearance: auto; font-size: .85rem; } .toolbar .sort-chevron { display: none; } }
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
  <div class="shell" id="dashboard">
    <p id="refresh-error" class="notice error" role="status" hidden>Updates unavailable. Displayed data may be out of date. Retrying automatically.</p>
    {{if .NotFound}}
      <header class="topline">
        <div><h1>Check not found</h1></div>
        {{template "themeControl"}}
      </header>
      <main id="main">
        <p class="muted">This page does not match a configured check.</p>
        <a class="back" href="/">← All checks</a>
      </main>
    {{else if .HasCurrent}}
      <a class="back" href="/">← All checks</a>
      <header class="topline">
        <div><h1>{{.Current.Name}}</h1>{{if .Current.Description}}<p class="check-description">{{.Current.Description}}</p>{{end}}</div>
        <div class="detail-status" id="detail-status">{{template "themeControl"}}<span class="badge status-{{statusClass .Current.Status}}">{{.Current.Status}}</span></div>
      </header>
      <main id="main">
        {{if .Info.StorageError}}<div class="notice error" role="alert">{{.Info.StorageError}}</div>{{end}}
        {{if .Info.ReloadError}}<div class="notice error" role="alert"><strong>Configuration reload issue:</strong> {{.Info.ReloadError}}</div>{{end}}
        <section id="check-details" class="panel detail-panel" aria-label="Check details">
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
        <section id="events" aria-labelledby="events-heading">
          <div class="section-heading"><h2 id="events-heading">Event history</h2><p class="muted">Recent pings: {{len .Current.Events}} · newest first · cleared on restart</p></div>
          {{if .Current.Events}}
            <ol class="event-list panel">
              {{range .Current.Events}}
                <li class="event {{.Kind}}" id="event-{{.ID}}">
                  <div class="event-top">
                    <div><strong>{{eventLabel .Kind}}</strong>{{if .Duration}}<span>Duration: {{formatDuration .Duration}}</span>{{end}}{{if .ExitCode}}<span>Exit code: {{.ExitCode}}</span>{{end}}</div>
                    {{template "timestamp" (timestamp .Time $.Now)}}
                  </div>
                  {{if or .Body .RID .Truncated}}<details><summary>{{if .Body}}Log details{{else}}Run details{{end}}</summary>
                    {{if or .RID .Truncated}}<div class="event-meta">{{if .RID}}<span>RID: {{.RID}}</span>{{end}}{{if .Truncated}}<span>Log was truncated</span>{{end}}</div>{{end}}
                    {{if .Body}}<pre>{{.Body}}</pre>{{end}}
                  </details>{{end}}
                </li>
              {{end}}
            </ol>
          {{else}}<p class="empty">No events have been received for this check.</p>{{end}}
        </section>

      </main>
    {{else}}
      <header class="topline">
        <div><h1>{{.Info.SiteName}}</h1></div>
        {{template "themeControl"}}
      </header>
      <main id="main">
        <div class="summary-bar" id="summary-bar">
        <section class="summary-grid" aria-label="Filter checks by status">
          <button class="metric" type="button" data-filter="all" aria-pressed="true"><span class="metric-label">Checks</span><span class="metric-value">{{.Total}}</span></button>
          <button class="metric" type="button" data-filter="up" aria-pressed="false"><span class="metric-label">Up</span><span class="metric-value status-up">{{.Up}}</span></button>
          <button class="metric" type="button" data-filter="down" aria-pressed="false"><span class="metric-label">Down</span><span class="metric-value status-down">{{.Down}}</span></button>
          <button class="metric" type="button" data-filter="grace" aria-pressed="false"><span class="metric-label">In grace</span><span class="metric-value status-grace">{{.Grace}}</span></button>
          <button class="metric" type="button" data-filter="awaiting" aria-pressed="false"><span class="metric-label">Awaiting</span><span class="metric-value">{{.Awaiting}}</span></button>
          <button class="metric" type="button" data-filter="paused" aria-pressed="false"><span class="metric-label">Paused</span><span class="metric-value">{{.Paused}}</span></button>
        </section>
        <div class="toolbar"><button class="sort-control" id="sort-direction" type="button" data-direction="asc" aria-label="Sort descending" data-full="Sort descending"><svg viewBox="0 0 24 24" aria-hidden="true"><path class="sort-down" d="M8 4v16m-4-4 4 4 4-4"/><path class="sort-up" d="M16 20V4m-4 4 4-4 4 4"/></svg></button><select id="sort-select" aria-label="Sort by"><option value="state">State</option><option value="name">Name</option><option value="due">Next due</option></select><svg class="sort-chevron" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg></div>
        </div>
        {{if .Info.StorageError}}<div class="notice error" role="alert">{{.Info.StorageError}}</div>{{end}}
        {{if .Info.ReloadError}}<div class="notice error" role="alert"><strong>Configuration reload issue:</strong> {{.Info.ReloadError}}</div>{{end}}
        {{if .Checks}}
          <section class="check-grid" id="check-grid" aria-label="Configured checks">
            {{range .Checks}}
              <article class="check-card status-{{statusClass .Status}}" id="check-{{.Key}}" data-status="{{.Status}}" data-name="{{.Name}}" data-due="{{if ne .Status "paused"}}{{timeAttr .NextDue}}{{end}}">
                <div class="card-top"><h2 class="check-name"><a class="check-link" href="/checks/{{.Key}}"{{if .Description}} aria-describedby="description-{{.Key}}"{{end}}>{{.Name}}</a>{{if .Description}}<span class="description-tooltip" id="description-{{.Key}}" role="tooltip">{{.Description}}</span>{{end}}</h2><span class="badge status-{{statusClass .Status}}">{{.Status}}</span></div>
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
{{define "themeControl"}}<div class="theme-switch" id="theme-switch" role="group" aria-label="Theme" data-choice="system">
  <button type="button" data-theme-choice="system" aria-label="System theme" data-full="System theme" aria-pressed="true"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8M12 17v4"/></svg></button>
  <button type="button" data-theme-choice="light" aria-label="Light theme" data-full="Light theme" aria-pressed="false"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5"/></svg></button>
  <button type="button" data-theme-choice="dark" aria-label="Dark theme" data-full="Dark theme" aria-pressed="false"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20.5 14A9 9 0 0 1 10 3.5 9 9 0 1 0 20.5 14Z"/></svg></button>
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
  const selectTheme = (theme = document.documentElement.dataset.theme || "system") => {
    const themeSwitch = document.querySelector(".theme-switch");
    themeSwitch.dataset.choice = theme;
    themeSwitch.querySelectorAll("button").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.themeChoice === theme)));
    if (theme === "system") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
  };
  selectTheme();
  document.addEventListener("click", event => {
    const button = event.target.closest("[data-theme-choice]");
    if (!button) return;
    selectTheme(button.dataset.themeChoice);
    savePreference("hc-theme", button.dataset.themeChoice);
  });
  let updateChecks = () => {};
  const sort = document.getElementById("sort-select");
  if (sort) {
    const directionButton = document.getElementById("sort-direction");
    let direction = readPreference("hc-sort-direction") === "desc" ? -1 : 1;
    const filters = Array.from(document.querySelectorAll("[data-filter]"));
    const savedSort = readPreference("hc-sort");
    if (["state", "name", "due"].includes(savedSort)) sort.value = savedSort;
    let status = readPreference("hc-status") || "all";
    if (!filters.some(button => button.dataset.filter === status)) status = "all";
    const ranks = { down: 0, grace: 1, awaiting: 2, up: 3, paused: 4 };
    const names = new Intl.Collator("en", { numeric: true, sensitivity: "base" });
    updateChecks = (root = document) => {
      const grid = root.querySelector(".check-grid");
      const cards = grid ? Array.from(grid.querySelectorAll(".check-card")) : [];
      cards.sort((a, b) => {
        let order = 0;
        if (sort.value === "state") order = ranks[a.dataset.status] - ranks[b.dataset.status];
        if (sort.value === "due") {
          // Checks without an active deadline stay last in either direction.
          const missing = Number(!a.dataset.due) - Number(!b.dataset.due);
          if (missing) return missing;
          if (a.dataset.due) order = Date.parse(a.dataset.due) - Date.parse(b.dataset.due);
        }
        return direction * (order || names.compare(a.dataset.name, b.dataset.name));
      });
      directionButton.dataset.direction = direction === 1 ? "asc" : "desc";
      const action = direction === 1 ? "Sort descending" : "Sort ascending";
      directionButton.setAttribute("aria-label", action);
      directionButton.dataset.full = action;
      cards.forEach((card, index) => {
        card.hidden = status !== "all" && card.dataset.status !== status;
        if (grid.children[index] !== card) grid.insertBefore(card, grid.children[index] || null);
      });
      root.querySelectorAll("[data-filter]").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.filter === status)));
      const empty = root.querySelector("#filter-empty");
      if (empty) empty.hidden = cards.some(card => !card.hidden);
    };
    sort.addEventListener("change", () => { savePreference("hc-sort", sort.value); updateChecks(); });
    directionButton.addEventListener("click", () => {
      direction *= -1;
      savePreference("hc-sort-direction", direction === 1 ? "asc" : "desc");
      updateChecks();
    });
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
      if (element.dataset.full !== fullTime) element.dataset.full = fullTime;
      if (element.getAttribute("aria-label") !== fullTime) element.setAttribute("aria-label", fullTime);
      const seconds = (date.getTime() - Date.now()) / 1000;
      const [size, unit] = Math.abs(seconds) >= 84600 ? [86400, "day"] : Math.abs(seconds) >= 3570 ? [3600, "hour"] : Math.abs(seconds) >= 60 ? [60, "minute"] : [1, "second"];
      const amount = Math.sign(seconds) * Math.round(Math.abs(seconds) / size);
      const text = amount === 0 ? "now" : formatter.format(amount, unit);
      const label = element.dataset.due === "true" ? text.replace(/ ago$/, " overdue") : text;
      if (element.textContent !== label) element.textContent = label;
    });
  };
  updateTimes();
  const positionTooltip = element => {
    const rect = element.getBoundingClientRect();
    const viewport = document.documentElement;
    element.style.setProperty("--tooltip-max-width", viewport.clientWidth - 32 + "px");
    const tooltip = element.matches(".check-link") ? document.getElementById(element.getAttribute("aria-describedby")) : element;
    const style = getComputedStyle(tooltip, tooltip === element ? "::after" : null);
    const width = parseFloat(style.width);
    const height = parseFloat(style.height);
    const left = Math.max(16, Math.min(rect.left, viewport.clientWidth - width - 16));
    const above = rect.top - height - 6;
    const top = Math.max(16, Math.min(above >= 16 ? above : rect.bottom + 6, viewport.clientHeight - height - 16));
    tooltip.style.setProperty("--tooltip-left", left + "px");
    tooltip.style.setProperty("--tooltip-top", top + "px");
    tooltip.dataset.positioned = "true";
  };
  // Delegation also covers timestamps and descriptions added by a refresh.
  for (const type of ["mouseover", "focusin"]) document.addEventListener(type, event => {
    const trigger = event.target.closest("[data-full], .check-link[aria-describedby]");
    if (trigger && !trigger.contains(event.relatedTarget)) positionTooltip(trigger);
  });
  for (const type of ["mouseout", "focusout"]) document.addEventListener(type, event => {
    const trigger = event.target.closest("[data-full], .check-name");
    if (!trigger || trigger.contains(event.relatedTarget)) return;
    const tooltip = trigger.matches(".check-name") ? trigger.querySelector(".description-tooltip") : trigger;
    if (tooltip && !trigger.contains(document.activeElement) && !trigger.matches(":hover")) delete tooltip.dataset.dismissed;
  });
  document.addEventListener("keydown", event => {
    if (event.key === "Escape") document.querySelectorAll("[data-full]:hover, [data-full]:focus, .check-name:hover .description-tooltip, .check-name:focus-within .description-tooltip").forEach(tooltip => { tooltip.dataset.dismissed = "true"; });
  });
  const positionVisibleTooltips = () => document.querySelectorAll("[data-full]:hover, [data-full]:focus, .check-link[aria-describedby]:hover, .check-link[aria-describedby]:focus").forEach(positionTooltip);
  window.addEventListener("resize", positionVisibleTooltips);
  window.addEventListener("scroll", positionVisibleTooltips, true);
  window.setInterval(updateTimes, 1000);

  // Reuse keyed cards/events and their children so focus, selection, expanded
  // logs and log scroll positions survive. Only changed text/attributes are set.
  const syncNode = (current, next) => {
    if (current.nodeType !== Node.ELEMENT_NODE) {
      if (current.nodeValue !== next.nodeValue) current.nodeValue = next.nodeValue;
      return;
    }
    if (current.matches(".theme-switch, .toolbar, #refresh-error")) return;
    const clientAttribute = name => name === "open" || name === "style" || name === "data-dismissed" || name === "data-positioned" ||
      (current.matches("time") && (name === "data-full" || name === "aria-label"));
    for (const attribute of Array.from(current.attributes)) {
      if (!clientAttribute(attribute.name) && !next.hasAttribute(attribute.name)) current.removeAttribute(attribute.name);
    }
    for (const attribute of next.attributes) {
      if (!clientAttribute(attribute.name) && current.getAttribute(attribute.name) !== attribute.value) current.setAttribute(attribute.name, attribute.value);
    }
    // updateTimes owns the displayed text, using the refreshed datetime.
    if (current.matches("time")) return;
    const remaining = Array.from(current.childNodes);
    let cursor = current.firstChild;
    for (const child of Array.from(next.childNodes)) {
      const index = remaining.findIndex(node => node.id === child.id && node.nodeType === child.nodeType && node.nodeName === child.nodeName);
      const match = index < 0 ? child : remaining.splice(index, 1)[0];
      if (match !== cursor) current.insertBefore(match, cursor);
      if (index >= 0) syncNode(match, child);
      cursor = match.nextSibling;
    }
    for (const node of remaining) node.remove();
  };
  let refreshing = false;
  const refreshDashboard = async () => {
    if (refreshing) return;
    refreshing = true;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    const errorNotice = document.getElementById("refresh-error");
    try {
      const response = await fetch(window.location.href, { cache: "no-store", redirect: "error", signal: controller.signal });
      if ((!response.ok && response.status !== 404) || !response.headers.get("Content-Type")?.startsWith("text/html")) throw new Error("Unexpected dashboard response: " + response.status);
      const next = new DOMParser().parseFromString(await response.text(), "text/html");
      const dashboard = next.getElementById("dashboard");
      if (!dashboard || !next.getElementById("main") || !next.querySelector(".theme-switch")) throw new Error("Dashboard markup is missing");
      // Sort/filter the detached document first to avoid moving focused cards
      // back to server order before restoring the user's preferred order.
      updateChecks(next);
      const x = window.scrollX, y = window.scrollY;
      const focused = document.activeElement;
      const selection = document.getSelection();
      const selected = selection && !selection.isCollapsed ? [selection.anchorNode, selection.anchorOffset, selection.focusNode, selection.focusOffset] : null;
      syncNode(document.getElementById("dashboard"), dashboard);
      document.title = next.title;
      selectTheme();
      updateTimes();
      // Moving an existing node with insertBefore can clear browser focus and
      // selection. Restore them only if the original content still exists.
      if (focused.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true });
      const validPoint = (node, offset) => node.isConnected && offset <= (node.length ?? node.childNodes.length);
      if (selected && validPoint(selected[0], selected[1]) && validPoint(selected[2], selected[3])) selection.setBaseAndExtent(...selected);
      window.scrollTo(x, y);
      positionVisibleTooltips();
      errorNotice.hidden = true;
    } catch (error) {
      errorNotice.hidden = false;
      console.warn("Dashboard refresh failed.", error);
    } finally {
      window.clearTimeout(timeout);
      refreshing = false;
    }
  };
  window.setInterval(refreshDashboard, 15000);
})();`

func renderDashboard(w http.ResponseWriter, r *http.Request, e *Engine) {
	setDashboardHeaders(w)
	data := dashboardData{Script: template.JS(dashboardRefreshScript), ThemeScript: template.JS(dashboardThemeScript), Now: e.now()}
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
	refreshScriptHash := sha256.Sum256([]byte(dashboardRefreshScript))
	themeScriptHash := sha256.Sum256([]byte(dashboardThemeScript))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(refreshScriptHash[:])+"' 'sha256-"+base64.StdEncoding.EncodeToString(themeScriptHash[:])+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
