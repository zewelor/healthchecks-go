package healthchecks

import (
	"context"
	"crypto/rand"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

type Input struct {
	Kind      string
	Body      string
	RID       string
	ExitCode  *int
	Truncated bool
}

type Event struct {
	Time      time.Time
	Kind      string
	Body      string
	RID       string
	ExitCode  *int
	Duration  *time.Duration
	Truncated bool
}

type CheckView struct {
	Key, Name, Status, Reason, Schedule               string
	DependsOnRegions                                  []string
	LastSuccess, LastPing, Started, NextDue, Deadline time.Time
	Duration                                          *time.Duration
	Events                                            []Event
	Notify                                            bool
	DeliveryError                                     string
}

type ServerView struct {
	Started                            time.Time
	ReloadError, WebhookName, SiteName string
	StorageError                       string
}

type checkState struct {
	incarnation                             string
	pending                                 *pendingDelivery
	config                                  CheckConfig
	observed                                time.Time
	lastSuccess, lastPing, started, nextDue time.Time
	startRID                                string
	duration                                *time.Duration
	status, reason                          string
	down                                    bool
	history                                 []Event
	notifiedDown                            bool
	deliveryError                           string
	attempts                                int
	nextAttempt                             time.Time
	failedRevision                          uint64
	revision                                uint64
	flight                                  *delivery
}

type Engine struct {
	mu                   sync.Mutex
	checks               map[string]*checkState
	now                  func() time.Time
	started              time.Time
	publicURL            string
	webhook              *WebhookConfig
	webhookNotBefore     time.Time
	reloadError          string
	storageError         string
	store                *postgresStore
	applied              *Config
	targetHash, bodyHash string
	wake                 chan struct{}
}

func NewEngine(c *Config, now func() time.Time) *Engine {
	e := &Engine{checks: make(map[string]*checkState), now: now, started: now(), wake: make(chan struct{}, 1)}
	e.Reload(c)
	return e
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) Reload(c *Config) error {
	return e.transact(context.Background(), func() { e.reloadLocked(c) })
}

func (e *Engine) reloadLocked(c *Config) {
	now := e.now()
	checks := make(map[string]*checkState, len(c.Checks))
	target, body := webhookHashes(c.Webhook)
	webhookChanged := e.targetHash != target
	bodyChanged := e.bodyHash != body
	e.targetHash, e.bodyHash = target, body
	if webhookChanged {
		e.webhookNotBefore = time.Time{}
	}
	for _, definition := range c.Checks {
		definition.DependsOnRegions = append([]string{}, definition.DependsOnRegions...)
		s := e.checks[definition.UUID]
		if s == nil {
			s = &checkState{incarnation: rand.Text(), observed: now, status: "awaiting", revision: 1}
		}
		if s.config.Paused && !definition.Paused {
			s.observed = now
			s.lastSuccess = time.Time{}
			s.lastPing = time.Time{}
			s.started = time.Time{}
			s.startRID = ""
			s.down = false
			s.reason = ""
			s.duration = nil
			s.notifiedDown = false
		}
		resetDelivery := webhookChanged || !definition.Notify || definition.Paused
		if resetDelivery || bodyChanged {
			if resetDelivery {
				if s.flight != nil {
					s.flight = nil
				}
				s.notifiedDown = false
				s.pending = nil
			}
			// A new payload supersedes old failures, while an in-flight success
			// at the same recipient still counts as a delivered notification.
			s.revision++
			s.attempts = 0
			s.nextAttempt = time.Time{}
			s.failedRevision = 0
			s.deliveryError = ""
		}
		s.config = definition
		base := s.lastSuccess
		if base.IsZero() {
			base = s.observed
		}
		s.nextDue = definition.next(base)
		checks[definition.UUID] = s
	}
	e.checks = checks
	e.publicURL = c.PublicURL
	e.webhook = c.Webhook
	e.applied = c
	e.reloadError = ""
	e.evaluateLocked(now)
	e.signal()
}

func (e *Engine) SetReloadError(message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reloadError = message
}

func (e *Engine) Info() ServerView {
	e.mu.Lock()
	defer e.mu.Unlock()
	siteName := "Healthchecks"
	if e.applied != nil && e.applied.SiteName != "" {
		siteName = e.applied.SiteName
	}
	v := ServerView{Started: e.started, ReloadError: e.reloadError, SiteName: siteName, StorageError: e.storageError}
	if e.webhook != nil {
		v.WebhookName = e.webhook.Name
	}
	return v
}

func (e *Engine) Ping(id string, in Input) (bool, error) {
	found := false
	var attrs []slog.Attr
	err := e.transact(context.Background(), func() {
		found = e.pingLocked(id, in)
		if found {
			s := e.checks[id]
			attrs = []slog.Attr{
				slog.String("check", s.config.Name),
				slog.String("key", s.config.Key),
				slog.String("kind", in.Kind),
				slog.String("status", s.status),
				slog.Bool("ignored", s.config.Paused),
			}
			if in.ExitCode != nil {
				attrs = append(attrs, slog.Int("exit_code", *in.ExitCode))
			}
		}
	})
	if err != nil {
		slog.Error("Ping could not be saved", "error", err)
	} else if found {
		slog.LogAttrs(context.Background(), slog.LevelInfo, "Ping accepted", attrs...)
	}
	return found, err
}

func (e *Engine) pingLocked(id string, in Input) bool {
	s := e.checks[id]
	if s == nil {
		return false
	}
	now := e.now()
	e.evaluateLocked(now)
	var duration *time.Duration
	if !s.config.Paused {
		switch in.Kind {
		case "start":
			s.started = now
			s.startRID = in.RID
		case "success", "fail":
			s.lastPing = now
			s.duration = nil
			if !s.started.IsZero() {
				if s.startRID == in.RID {
					d := now.Sub(s.started)
					duration = &d
					s.duration = &d
					s.started = time.Time{}
				}
				if in.Kind == "fail" || in.RID == "" {
					s.started = time.Time{}
				}
			}
			if in.Kind == "success" {
				s.lastSuccess = now
				s.nextDue = s.config.next(now)
				s.down = false
				s.reason = ""
			} else {
				s.down = true
				s.reason = "Explicit failure signal."
			}
		}
	}
	if len(in.Body) > BodyLimit {
		in.Body = strings.Clone(in.Body[:BodyLimit])
		in.Truncated = true
	}
	event := Event{Time: now, Kind: in.Kind, Body: in.Body, RID: in.RID, Duration: duration, Truncated: in.Truncated}
	if in.ExitCode != nil {
		code := *in.ExitCode
		event.ExitCode = &code
	}
	if s.config.Paused {
		event.Kind = "ignored"
	}
	if len(s.history) == HistoryLimit {
		copy(s.history, s.history[1:])
		s.history[len(s.history)-1] = event
	} else {
		s.history = append(s.history, event)
	}
	e.evaluateLocked(now)
	e.signal()
	return true
}

func (e *Engine) Evaluate() error {
	return e.transact(context.Background(), func() { e.evaluateLocked(e.now()) })
}

func (e *Engine) evaluateLocked(now time.Time) {
	for _, s := range e.checks {
		status := "up"
		if s.config.Paused {
			status = "paused"
		} else {
			if !s.down {
				if !s.started.IsZero() && !now.Before(s.started.Add(s.config.grace)) {
					s.down = true
					s.reason = "Execution did not finish within grace."
				}
				if !now.Before(s.nextDue.Add(s.config.grace)) {
					s.down = true
					s.reason = "No successful ping before the deadline."
				}
			}
			if s.down {
				status = "down"
			} else if !now.Before(s.nextDue) {
				status = "grace"
			} else if s.lastSuccess.IsZero() {
				status = "awaiting"
			}
		}
		if status != s.status {
			// Grace changes the displayed status, not the notification to deliver.
			if (status == "down") != (s.status == "down") {
				s.revision++
				s.attempts = 0
				s.nextAttempt = time.Time{}
				s.failedRevision = 0
				s.deliveryError = ""
			}
			s.status = status
			e.signal()
		}
	}
}

func (e *Engine) Snapshot() []CheckView {
	views, _ := e.snapshot()
	return views
}

func (e *Engine) snapshot() ([]CheckView, error) {
	err := e.Evaluate()
	e.mu.Lock()
	defer e.mu.Unlock()
	views := make([]CheckView, 0, len(e.checks))
	for _, s := range e.checks {
		views = append(views, s.view())
	}
	sort.Slice(views, func(i, j int) bool {
		if (views[i].Status == "down") != (views[j].Status == "down") {
			return views[i].Status == "down"
		}
		if views[i].Name == views[j].Name {
			return views[i].Key < views[j].Key
		}
		return strings.ToLower(views[i].Name) < strings.ToLower(views[j].Name)
	})
	return views, err
}

func (s *checkState) view() CheckView {
	c := s.config
	schedule := "Every " + formatScheduleInterval(c.interval)
	if c.Cron != "" {
		schedule = c.Cron + " (" + c.Timezone + ")"
	}
	deadline := s.nextDue.Add(c.grace)
	if !s.started.IsZero() && s.started.Add(c.grace).Before(deadline) {
		deadline = s.started.Add(c.grace)
	}
	events := make([]Event, len(s.history))
	for i, event := range s.history {
		events[len(events)-1-i] = event
	}
	return CheckView{Key: c.Key, Name: c.Name, DependsOnRegions: append([]string{}, c.DependsOnRegions...),
		Status: s.status, Reason: s.reason, Schedule: schedule, LastSuccess: s.lastSuccess, LastPing: s.lastPing, Started: s.started,
		NextDue: s.nextDue, Deadline: deadline, Duration: s.duration, Events: events, Notify: c.Notify, DeliveryError: s.deliveryError}
}
