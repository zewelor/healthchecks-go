package healthchecks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const databaseTimeout = 5 * time.Second

var (
	errDatabaseOwned     = errors.New("another healthchecks-go instance owns this database")
	errUnsupportedSchema = errors.New("unsupported database schema version")
)

// One connection, serialized by Engine.mu, also owns the session advisory lock.
// Losing it requires reacquiring ownership and reloading committed state.
type postgresStore struct {
	config *pgx.ConnConfig
	conn   *pgx.Conn
}

type pendingDelivery struct {
	ID       string
	Revision uint64
	Status   string
	Body     []byte
}

type storedCheck struct {
	Incarnation                                       string
	Paused                                            bool
	Observed, LastSuccess, LastPing, Started, NextDue time.Time
	StartRID                                          string
	Duration                                          *time.Duration
	Status, Reason                                    string
	Down, NotifiedDown                                bool
	DeliveryError                                     string
	Attempts                                          int
	NextAttempt                                       time.Time
	FailedRevision, Revision                          uint64
	Pending                                           *pendingDelivery
}

type storedRuntime struct {
	TargetHash, BodyHash string
	WebhookNotBefore     time.Time
}

type storedState struct {
	Runtime []byte
	Checks  map[string][]byte
}

// OpenEngine restores and reconciles YAML before any worker or HTTP handler runs.
// There is deliberately no in-memory fallback if PostgreSQL is unavailable.
func OpenEngine(ctx context.Context, c *Config, now func() time.Time, lookup func(string) (string, bool)) (*Engine, error) {
	config, err := databaseConfig(lookup)
	if err != nil {
		return nil, err
	}
	e := &Engine{checks: make(map[string]*checkState), now: now, started: now(), wake: make(chan struct{}, 1), store: &postgresStore{config: config}}
	if err := e.transact(ctx, func() { e.reloadLocked(c) }); err != nil {
		_ = e.Close()
		return nil, err
	}
	return e, nil
}

func databaseConfig(lookup func(string) (string, bool)) (*pgx.ConnConfig, error) {
	values := make(map[string]string)
	for _, key := range []string{"DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD"} {
		values[key], _ = lookup(key)
		if values[key] == "" {
			return nil, fmt.Errorf("%s is required", key)
		}
	}
	port, _ := lookup("DB_PORT")
	if port == "" {
		port = "5432"
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return nil, errors.New("DB_PORT must be between 1 and 65535")
	}
	sslmode, _ := lookup("DB_SSLMODE")
	if sslmode == "" {
		sslmode = "require"
	}
	switch sslmode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		return nil, errors.New("DB_SSLMODE must be disable, require, verify-ca or verify-full")
	}
	// Quote before parsing so TLS hostname and fallback settings are built from
	// the actual host, and passwords containing spaces, quotes or slashes work.
	quote := func(s string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'" }
	dsn := "host=" + quote(values["DB_HOST"]) + " port=" + port + " dbname=" + quote(values["DB_NAME"]) + " user=" + quote(values["DB_USER"]) + " password=" + quote(values["DB_PASSWORD"]) + " sslmode=" + sslmode + " connect_timeout=5 application_name=healthchecks-go"
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database connection settings")
	}
	return config, nil
}

func (s *postgresStore) begin(ctx context.Context) (pgx.Tx, bool, error) {
	fresh := s.conn == nil || s.conn.IsClosed()
	if fresh {
		conn, err := pgx.ConnectConfig(ctx, s.config)
		if err != nil {
			return nil, true, errors.New("cannot connect to PostgreSQL")
		}
		s.conn = conn
		var owned bool
		err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended(current_database() || ':healthchecks_go', 0))`).Scan(&owned)
		if err != nil {
			return nil, true, errors.New("cannot acquire PostgreSQL ownership lock")
		}
		if !owned {
			return nil, true, errDatabaseOwned
		}
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fresh, errors.New("cannot begin state transaction")
	}
	if fresh {
		// The application uses its own database and the standard public schema.
		// Never change database/role defaults or unrelated tables.
		_, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.runtime (
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 version integer NOT NULL,
 state jsonb NOT NULL
);
INSERT INTO public.runtime (id, version, state) VALUES (true, 1, '{}') ON CONFLICT (id) DO NOTHING;`)
		if err == nil {
			var version int
			err = tx.QueryRow(ctx, `SELECT version FROM public.runtime WHERE id = true FOR UPDATE`).Scan(&version)
			if err == nil && version != 1 {
				err = fmt.Errorf("%w: %d", errUnsupportedSchema, version)
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.check_state (uuid text PRIMARY KEY, state jsonb NOT NULL)`)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, fresh, fmt.Errorf("cannot initialize database tables: %w", err)
		}
	}
	return tx, fresh, nil
}

func (e *Engine) encodedState() storedState {
	r, _ := json.Marshal(storedRuntime{e.targetHash, e.bodyHash, e.webhookNotBefore})
	data := storedState{Runtime: r, Checks: make(map[string][]byte, len(e.checks))}
	for id, s := range e.checks {
		data.Checks[id], _ = json.Marshal(storedCheck{
			Incarnation: s.incarnation, Paused: s.config.Paused, Observed: s.observed, LastSuccess: s.lastSuccess, LastPing: s.lastPing, Started: s.started, NextDue: s.nextDue, StartRID: s.startRID, Duration: s.duration, Status: s.status, Reason: s.reason, Down: s.down, NotifiedDown: s.notifiedDown, DeliveryError: s.deliveryError, Attempts: s.attempts, NextAttempt: s.nextAttempt, FailedRevision: s.failedRevision, Revision: s.revision, Pending: s.pending,
		})
	}
	return data
}

func (e *Engine) restore(ctx context.Context, tx pgx.Tx) error {
	var raw []byte
	var runtime storedRuntime
	if err := tx.QueryRow(ctx, `SELECT state FROM public.runtime WHERE id = true FOR UPDATE`).Scan(&raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &runtime); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT uuid, state FROM public.check_state FOR UPDATE`)
	if err != nil {
		return err
	}
	defer rows.Close()
	checks := make(map[string]*checkState)
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return err
		}
		var s storedCheck
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if !uuidPattern.MatchString(id) || s.Incarnation == "" || s.Revision == 0 || s.Observed.IsZero() ||
			(s.Pending != nil && (s.Pending.ID == "" || (s.Pending.Status != "down" && s.Pending.Status != "up") || !json.Valid(s.Pending.Body))) {
			return errors.New("invalid persisted check state")
		}
		current := &checkState{incarnation: s.Incarnation, config: CheckConfig{Paused: s.Paused}, observed: s.Observed, lastSuccess: s.LastSuccess, lastPing: s.LastPing, started: s.Started, nextDue: s.NextDue, startRID: s.StartRID, duration: s.Duration, status: s.Status, reason: s.Reason, down: s.Down, notifiedDown: s.NotifiedDown, deliveryError: s.DeliveryError, attempts: s.Attempts, nextAttempt: s.NextAttempt, failedRevision: s.FailedRevision, revision: s.Revision, pending: s.Pending}
		if old := e.checks[id]; old != nil && old.incarnation == s.Incarnation {
			current.history = old.history
			if old.flight != nil && s.Pending != nil && old.flight.token == s.Pending.ID {
				current.flight = old.flight
			}
		}
		checks[id] = current
	}
	if err := rows.Err(); err != nil {
		return err
	}
	e.checks = checks
	e.targetHash, e.bodyHash, e.webhookNotBefore = runtime.TargetHash, runtime.BodyHash, runtime.WebhookNotBefore
	return nil
}

func saveState(ctx context.Context, tx pgx.Tx, before, after storedState) error {
	if !bytes.Equal(before.Runtime, after.Runtime) {
		if _, err := tx.Exec(ctx, `UPDATE public.runtime SET state = $1 WHERE id = true`, after.Runtime); err != nil {
			return err
		}
	}
	for id, data := range after.Checks {
		if bytes.Equal(before.Checks[id], data) {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.check_state (uuid, state) VALUES ($1, $2) ON CONFLICT (uuid) DO UPDATE SET state = EXCLUDED.state`, id, data); err != nil {
			return err
		}
	}
	for id := range before.Checks {
		if _, exists := after.Checks[id]; !exists {
			if _, err := tx.Exec(ctx, `DELETE FROM public.check_state WHERE uuid = $1`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// cloneChecks preserves transient request pointers, but isolates mutable history.
func cloneChecks(checks map[string]*checkState) map[string]*checkState {
	copy := make(map[string]*checkState, len(checks))
	for id, s := range checks {
		value := *s
		value.history = append([]Event(nil), s.history...)
		copy[id] = &value
	}
	return copy
}

func cancelDetached(before, after map[string]*checkState) {
	for id, s := range before {
		if s.flight != nil && (after[id] == nil || after[id].flight != s.flight) {
			s.flight.cancel()
		}
	}
}

// transact serializes local changes and publishes them only after COMMIT.
// On any database error the old view is restored and the connection discarded;
// the next operation must read PostgreSQL, including after an uncertain COMMIT.
func (e *Engine) transact(parent context.Context, mutate func()) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldChecks := e.checks
	if e.store == nil {
		// In-memory engines use the same mutation and cancellation rules.
		oldFlights := cloneChecks(e.checks)
		mutate()
		cancelDetached(oldFlights, e.checks)
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, databaseTimeout)
	defer cancel()
	e.checks = cloneChecks(oldChecks)
	oldRuntime := storedRuntime{e.targetHash, e.bodyHash, e.webhookNotBefore}
	oldConfig, oldURL, oldWebhook, oldReloadError := e.applied, e.publicURL, e.webhook, e.reloadError
	tx, fresh, err := e.store.begin(ctx)
	if err == nil {
		defer tx.Rollback(ctx)
		if fresh {
			err = e.restore(ctx, tx)
		}
		if err == nil {
			before := e.encodedState()
			if fresh && e.applied != nil {
				e.reloadLocked(e.applied)
			}
			mutate()
			err = saveState(ctx, tx, before, e.encodedState())
			if err == nil {
				err = tx.Commit(ctx)
			}
		}
	}
	if err != nil {
		cancelDetached(e.checks, oldChecks)
		e.checks = oldChecks
		e.targetHash, e.bodyHash, e.webhookNotBefore = oldRuntime.TargetHash, oldRuntime.BodyHash, oldRuntime.WebhookNotBefore
		e.applied, e.publicURL, e.webhook, e.reloadError = oldConfig, oldURL, oldWebhook, oldReloadError
		e.store.disconnect()
		e.storageError = "PostgreSQL state is unavailable; changes are not being accepted"
		// Do not include driver errors here: connection errors can expose passwords.
		return fmt.Errorf("PostgreSQL state transaction failed: %w", safeDatabaseError(err))
	}
	cancelDetached(oldChecks, e.checks)
	e.storageError = ""
	return nil
}

func safeDatabaseError(err error) error {
	if errors.Is(err, errDatabaseOwned) || errors.Is(err, errUnsupportedSchema) {
		return err
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		return fmt.Errorf("PostgreSQL error SQLSTATE %s", databaseError.Code)
	}
	return errors.New("database unavailable or state invalid")
}

func (s *postgresStore) disconnect() {
	if s.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.conn.Close(ctx)
		s.conn = nil
	}
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.checks {
		if s.flight != nil {
			s.flight.cancel()
		}
	}
	if e.store != nil {
		e.store.disconnect()
	}
	return nil
}

func (e *Engine) Ready() error { return e.transact(context.Background(), func() {}) }

func webhookHashes(w *WebhookConfig) (string, string) {
	if w == nil {
		return "", ""
	}
	hash := func(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
	return hash(w.URL), hash(w.Body)
}
