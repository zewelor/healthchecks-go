package healthchecks

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	webhookRequestTimeout  = 10 * time.Second
	webhookResponseLimit   = 4096
	maxWebhookMessageRunes = 1000
	webhookRetryBase       = time.Second
	webhookRetryMaximum    = 5 * time.Minute
)

// delivery identifies a persisted notification intent across state copies and reconnects.
type delivery struct {
	token, incarnation string
	completedAt        time.Time
	cancel             context.CancelFunc
	ctx                context.Context
	checkID            string
	revision           uint64
	status             string
	url                string
	body               []byte
}

// RunWebhook runs the engine's single webhook delivery worker until ctx is
// cancelled. The application starts exactly one worker for each engine.
func (e *Engine) RunWebhook(ctx context.Context) {
	client := &http.Client{
		Timeout: webhookRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Webhook URLs can contain credentials in their query string. Do not
			// forward those credentials to another host or expose redirect URLs.
			return http.ErrUseLastResponse
		},
	}
	for {
		if ctx.Err() != nil {
			return
		}
		d, wait := e.prepareDelivery(ctx)
		if d != nil {
			status, retryAfter, err := deliverWebhook(client, d)
			for e.finishDelivery(d, status, retryAfter, err) != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			continue
		}

		var timer *time.Timer
		var timerC <-chan time.Time
		if wait > 0 {
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-e.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		}
	}
}

// prepareDelivery evaluates current state and starts the next eligible
// request. It returns the shortest delay until a retry when nothing is ready.
func (e *Engine) prepareDelivery(parent context.Context) (*delivery, time.Duration) {
	var d *delivery
	var wait time.Duration
	err := e.transact(parent, func() { d, wait = e.prepareDeliveryLocked(parent) })
	if err != nil {
		return nil, time.Second
	}
	return d, wait
}

func (e *Engine) prepareDeliveryLocked(parent context.Context) (*delivery, time.Duration) {
	if parent.Err() != nil {
		return nil, 0
	}
	now := e.now()
	e.evaluateLocked(now)
	if now.Before(e.webhookNotBefore) {
		return nil, e.webhookNotBefore.Sub(now)
	}
	var earliest time.Time
	for id, s := range e.checks {
		if !s.config.Notify || s.config.Paused || e.webhook == nil || s.flight != nil {
			continue
		}
		if now.Before(s.nextAttempt) {
			if earliest.IsZero() || s.nextAttempt.Before(earliest) {
				earliest = s.nextAttempt
			}
			continue
		}
		if s.pending != nil {
			return e.startDelivery(parent, id, s), 0
		}
		status := ""
		switch {
		case s.status == "down" && !s.notifiedDown:
			status = "down"
		case (s.status == "up" || s.status == "grace") && s.notifiedDown:
			status = "up"
		default:
			continue
		}
		if s.failedRevision == s.revision {
			continue
		}

		detailURL := e.publicURL + "/checks/" + s.config.Key
		event := WebhookEvent{
			Status:           status,
			Name:             s.config.Name,
			Tags:             strings.Fields(s.config.Tags),
			UniqueKey:        s.config.Key,
			Region:           s.config.Region,
			DependsOnRegions: append([]string{}, s.config.DependsOnRegions...),
			Reason:           s.reason,
			Time:             now,
			URL:              detailURL,
			Message:          webhookMessage(status, s.config.Name, s.reason, detailURL),
		}
		body, err := e.webhook.render(event)
		if err != nil {
			s.deliveryError = "webhook payload could not be rendered"
			s.failedRevision = s.revision
			continue
		}
		s.pending = &pendingDelivery{ID: rand.Text(), Revision: s.revision, Status: status, Body: body}
		return e.startDelivery(parent, id, s), 0
	}
	if earliest.IsZero() {
		return nil, 0
	}
	return nil, earliest.Sub(now)
}

func (e *Engine) startDelivery(parent context.Context, id string, s *checkState) *delivery {
	requestCtx, cancel := context.WithCancel(parent)
	p := s.pending
	d := &delivery{cancel: cancel, ctx: requestCtx, checkID: id, incarnation: s.incarnation, token: p.ID, revision: p.Revision, status: p.Status, url: e.webhook.URL, body: p.Body}
	s.flight = d
	return d
}

func deliverWebhook(client *http.Client, d *delivery) (status int, retryAfter string, requestErr error) {
	req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, d.url, bytes.NewReader(d.body))
	if err != nil {
		// Config validation makes this unreachable for normal operation. Keep
		// the raw error private because it can contain the webhook URL.
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookResponseLimit))
	return resp.StatusCode, resp.Header.Get("Retry-After"), nil
}

func (e *Engine) finishDelivery(d *delivery, status int, retryAfter string, requestErr error) error {
	e.mu.Lock()
	if d.completedAt.IsZero() {
		d.completedAt = e.now()
	}
	e.mu.Unlock()
	return e.transact(context.Background(), func() { e.finishDeliveryLocked(d, status, retryAfter, requestErr) })
}

func (e *Engine) finishDeliveryLocked(d *delivery, status int, retryAfter string, requestErr error) {
	retryable := requestErr != nil || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	receiverDelay := time.Duration(-1)
	// Receiver-level feedback survives removal of the originating check. Ignore
	// responses from a previous URL and cancellations without an HTTP response.
	if retryable && requestErr == nil && e.webhook != nil && e.webhook.URL == d.url {
		receiverDelay = retryAfterDelay(retryAfter, d.completedAt)
		if receiverDelay >= 0 {
			if deadline := d.completedAt.Add(receiverDelay); deadline.After(e.webhookNotBefore) {
				e.webhookNotBefore = deadline
			}
		}
	}
	s := e.checks[d.checkID]
	if s == nil || s.incarnation != d.incarnation || s.pending == nil || s.pending.ID != d.token {
		d.cancel()
		return
	}
	wasCanceled := d.ctx.Err() != nil
	isSuccess := requestErr == nil && status >= http.StatusOK && status < http.StatusMultipleChoices
	s.flight = nil
	if wasCanceled && !isSuccess {
		// Shutdown cancellation is not a delivery failure. A future worker can
		// resume this state; a reload has already detached the flight above.
		e.signal()
		return
	}
	s.pending = nil
	if isSuccess {
		s.deliveryError = ""
		s.attempts = 0
		s.nextAttempt = time.Time{}
		s.failedRevision = 0
		if d.status == "down" {
			// Record the delivered status even if revision changed during the
			// request. The worker will then deliver recovery from current state.
			s.notifiedDown = true
		} else {
			s.notifiedDown = false
		}
		e.signal()
		return
	}

	if retryable {
		if s.revision != d.revision {
			// Do not count an old delivery failure against the newer state.
			// The receiver's shared pause still applies.
			e.signal()
			return
		}
		s.attempts++
		delay := retryDelay(s.attempts)
		if requestErr == nil {
			if receiverDelay >= 0 {
				delay = receiverDelay
			}
			s.deliveryError = fmt.Sprintf("webhook returned HTTP %d; retry scheduled", status)
		} else {
			s.deliveryError = "webhook request failed; retry scheduled"
		}
		s.nextAttempt = d.completedAt.Add(delay)
		e.signal()
		return
	}

	if s.revision == d.revision {
		if status != 0 {
			s.deliveryError = fmt.Sprintf("webhook returned HTTP %d", status)
		} else {
			s.deliveryError = "webhook request could not be created"
		}
		s.failedRevision = s.revision
	}
	e.signal()
}

func retryDelay(attempt int) time.Duration {
	delay := webhookRetryBase
	for i := 1; i < attempt && delay < webhookRetryMaximum; i++ {
		if delay > webhookRetryMaximum/2 {
			return webhookRetryMaximum
		}
		delay *= 2
	}
	if delay > webhookRetryMaximum {
		return webhookRetryMaximum
	}
	return delay
}

// retryAfterDelay returns -1 when the header is absent or invalid.
func retryAfterDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return -1
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		// Saturate only at time.Duration's representation limit, not the
		// exponential backoff cap: a recipient can request a longer pause.
		maximum := time.Duration(1<<63 - 1)
		if seconds > uint64(maximum/time.Second) {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return -1
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func webhookMessage(status, name, reason, detailURL string) string {
	var message string
	if status == "down" {
		message = name + " is down"
		if reason != "" {
			message += ": " + reason
		}
	} else {
		message = name + " recovered"
	}
	message += "\n" + detailURL
	return limitRunes(message, maxWebhookMessageRunes)
}

func limitRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
