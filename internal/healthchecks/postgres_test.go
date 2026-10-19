package healthchecks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const postgresLegacySentinelID = "healthchecks-go-integration"

type postgresFixture struct {
	dsn           string
	sentinelValue string
}

type postgresTestEngine struct {
	*Engine
}

func newPostgresFixture(t *testing.T) *postgresFixture {
	t.Helper()
	if os.Getenv("HC_TEST_POSTGRES") != "1" {
		t.Skip("set HC_TEST_POSTGRES=1 with DB_* set to a disposable local PostgreSQL database")
	}

	dsn := postgresTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to disposable PostgreSQL test database: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	// The opt-in test database is disposable. Reset only the application's
	// tables; leave unrelated tables, including the sentinel, in place.
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS public.check_state, public.runtime`); err != nil {
		t.Fatalf("reset application test tables: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.healthchecks_legacy_sentinel (
		id text PRIMARY KEY,
		value text NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy sentinel fixture: %v", err)
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("make test sentinel value: %v", err)
	}
	sentinelValue := hex.EncodeToString(random[:])
	if _, err := conn.Exec(ctx, `INSERT INTO public.healthchecks_legacy_sentinel (id, value)
		VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET value = EXCLUDED.value`, postgresLegacySentinelID, sentinelValue); err != nil {
		t.Fatalf("seed legacy sentinel fixture: %v", err)
	}

	return &postgresFixture{
		dsn:           dsn,
		sentinelValue: sentinelValue,
	}
}

func postgresTestDSN(t *testing.T) string {
	t.Helper()
	get := func(name string, required bool) string {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			if required {
				t.Fatalf("HC_TEST_POSTGRES=1 requires %s", name)
			}
			return ""
		}
		return value
	}

	host := get("DB_HOST", true)
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("refusing destructive PostgreSQL tests for non-local DB_HOST %q", host)
	}
	database := get("DB_NAME", true)
	if database != "hc_test" {
		t.Fatalf("refusing destructive PostgreSQL tests unless DB_NAME=hc_test (got %q)", database)
	}
	port := get("DB_PORT", false)
	if port == "" {
		port = "5432"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		t.Fatalf("invalid DB_PORT %q", port)
	}
	sslmode := get("DB_SSLMODE", false)
	if sslmode == "" {
		sslmode = "require"
	}
	query := url.Values{"sslmode": []string{sslmode}}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(get("DB_USER", true), get("DB_PASSWORD", true)),
		Host:     net.JoinHostPort(strings.Trim(host, "[]"), port),
		Path:     "/" + database,
		RawQuery: query.Encode(),
	}
	return u.String()
}

func (f *postgresFixture) open(t *testing.T, config string, now func() time.Time) *postgresTestEngine {
	t.Helper()
	c := parseTestConfig(t, config)
	e, err := OpenEngine(context.Background(), c, now, os.LookupEnv)
	if err != nil {
		t.Fatalf("open PostgreSQL engine: %v", err)
	}
	wrapped := &postgresTestEngine{Engine: e}
	t.Cleanup(func() { wrapped.close(t) })
	return wrapped
}

func (e *postgresTestEngine) close(t *testing.T) {
	t.Helper()
	if e.Engine == nil {
		return
	}
	if err := e.Engine.Close(); err != nil {
		t.Errorf("close PostgreSQL engine: %v", err)
	}
	e.Engine = nil
}

func (f *postgresFixture) assertLegacySentinel(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to check legacy sentinel: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var got string
	if err := conn.QueryRow(ctx, `SELECT value FROM public.healthchecks_legacy_sentinel WHERE id = $1`, postgresLegacySentinelID).Scan(&got); err != nil {
		t.Fatalf("read legacy sentinel: %v", err)
	}
	if got != f.sentinelValue {
		t.Fatalf("legacy sentinel changed: got %q, want %q", got, f.sentinelValue)
	}
}

func postgresTestUUID(t *testing.T) string {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	random[6] = (random[6] & 0x0f) | 0x40
	random[8] = (random[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(random[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

func postgresConfig(id, every, grace string, notify, paused bool, webhookURL string) string {
	config := strings.ReplaceAll(simpleConfig, testUUID, id)
	config = strings.Replace(config, "every: 5m", "every: "+every, 1)
	config = strings.Replace(config, "    grace: 1m", "    grace: "+grace, 1)
	var flags strings.Builder
	if notify {
		flags.WriteString("    notify: true\n")
	}
	if paused {
		flags.WriteString("    paused: true\n")
	}
	if flags.Len() > 0 {
		config = strings.Replace(config, "    grace:", flags.String()+"    grace:", 1)
	}
	if webhookURL != "" {
		config += "webhook:\n  name: integration\n  url: " + strconv.Quote(webhookURL) + "\n"
	}
	return config
}

func postgresPing(t *testing.T, e *Engine, id string, in Input) {
	t.Helper()
	ok, err := e.Ping(id, in)
	if err != nil {
		t.Fatalf("persist %s ping: %v", in.Kind, err)
	}
	if !ok {
		t.Fatalf("known check %s was not found", id)
	}
}

func postgresOnlyCheck(t *testing.T, e *Engine) CheckView {
	t.Helper()
	checks := e.Snapshot()
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want one: %+v", len(checks), checks)
	}
	return checks[0]
}

func TestPostgresRestartPersistsCheckAndWebhookRetryState(t *testing.T) {
	fixture := newPostgresFixture(t)
	id := postgresTestUUID(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(id, "24h", "1m", true, false, "https://example.test/hook")

	first := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, first.Engine, id, Input{Kind: "success"})
	due := postgresOnlyCheck(t, first.Engine).NextDue
	now = now.Add(23*time.Hour + 45*time.Second)
	postgresPing(t, first.Engine, id, Input{Kind: "start", RID: "run-42"})
	first.close(t)

	second := fixture.open(t, config, func() time.Time { return now })
	view := postgresOnlyCheck(t, second.Engine)
	if view.Status != "up" || !view.LastSuccess.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || !view.NextDue.Equal(due) {
		t.Fatalf("restart reset the long check interval: %+v, want due %s", view, due)
	}
	if !view.Started.Equal(now) {
		t.Fatalf("restart lost the active run start: got %s, want %s", view.Started, now)
	}
	now = now.Add(45 * time.Second)
	postgresPing(t, second.Engine, id, Input{Kind: "success", RID: "run-42"})
	view = postgresOnlyCheck(t, second.Engine)
	if view.Duration == nil || *view.Duration != 45*time.Second {
		t.Fatalf("RID completion after restart lost its duration: %+v", view)
	}
	postgresPing(t, second.Engine, id, Input{Kind: "fail"})
	second.close(t)

	third := fixture.open(t, config, func() time.Time { return now })
	if view := postgresOnlyCheck(t, third.Engine); view.Status != "down" {
		t.Fatalf("restart lost explicit down state: %+v", view)
	}
	downDelivery, _ := third.prepareDelivery(context.Background())
	if downDelivery == nil || downDelivery.status != "down" {
		t.Fatal("down notification was not available")
	}
	third.finishDelivery(downDelivery, 204, "", nil)
	postgresPing(t, third.Engine, id, Input{Kind: "success"})
	recovery, _ := third.prepareDelivery(context.Background())
	if recovery == nil || recovery.status != "up" {
		t.Fatal("recovery notification was not available")
	}
	third.finishDelivery(recovery, 503, "120", nil)
	third.close(t)

	now = now.Add(30 * time.Second)
	fourth := fixture.open(t, config, func() time.Time { return now })
	if next, wait := fourth.prepareDelivery(context.Background()); next != nil || wait != 90*time.Second {
		if next != nil {
			next.cancel()
		}
		t.Fatalf("restart bypassed the recovery Retry-After gate: delivery=%v wait=%s", next != nil, wait)
	}
	now = now.Add(90 * time.Second)
	retry, _ := fourth.prepareDelivery(context.Background())
	if retry == nil || retry.status != "up" {
		t.Fatal("recovery was not retried at the persisted deadline")
	}
	fourth.finishDelivery(retry, 204, "", nil)
	if next, _ := fourth.prepareDelivery(context.Background()); next != nil {
		next.cancel()
		t.Fatal("completed recovery was delivered again")
	}
	fourth.close(t)
	fixture.assertLegacySentinel(t)
}

func TestPostgresReplaysCapturedPayloadAfterUnknownSendOutcome(t *testing.T) {
	fixture := newPostgresFixture(t)
	id := postgresTestUUID(t)
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(id, "5m", "1m", true, false, "https://example.test/hook")
	first := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, first.Engine, id, Input{Kind: "fail"})
	original, _ := first.prepareDelivery(context.Background())
	if original == nil || original.status != "down" || len(original.body) == 0 {
		t.Fatal("missing captured outage payload")
	}
	originalBody := bytes.Clone(original.body)
	originalURL := original.url
	first.close(t) // The request may have reached the receiver before the process stopped.

	now = now.Add(20 * time.Second)
	second := fixture.open(t, config, func() time.Time { return now })
	replay, _ := second.prepareDelivery(context.Background())
	if replay == nil || replay.status != "down" || replay.url != originalURL || !bytes.Equal(replay.body, originalBody) {
		if replay != nil {
			replay.cancel()
		}
		t.Fatalf("restart regenerated or lost the uncertain delivery: got=%+v, body equal=%v", replay, replay != nil && bytes.Equal(replay.body, originalBody))
	}
	second.finishDelivery(replay, 204, "", nil)
	second.close(t)
	fixture.assertLegacySentinel(t)
}

func TestPostgresRemovedAndReaddedCheckRejectsStaleCompletion(t *testing.T) {
	fixture := newPostgresFixture(t)
	id := postgresTestUUID(t)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(id, "5m", "1m", true, false, "https://example.test/hook")
	e := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, e.Engine, id, Input{Kind: "fail"})
	oldDelivery, _ := e.prepareDelivery(context.Background())
	if oldDelivery == nil {
		t.Fatal("missing original outage delivery")
	}
	otherID := postgresTestUUID(t)
	removed := postgresConfig(otherID, "5m", "1m", true, false, "https://example.test/hook")
	if err := e.Reload(parseTestConfig(t, removed)); err != nil {
		t.Fatalf("remove check during delivery: %v", err)
	}
	if err := e.Reload(parseTestConfig(t, config)); err != nil {
		t.Fatalf("re-add check: %v", err)
	}
	e.finishDelivery(oldDelivery, 204, "", nil)
	view := postgresOnlyCheck(t, e.Engine)
	wantKey := parseTestConfig(t, config).Checks[0].Key
	if view.Key != wantKey || view.Status != "awaiting" {
		t.Fatalf("stale completion changed re-added check: %+v", view)
	}
	if next, _ := e.prepareDelivery(context.Background()); next != nil {
		next.cancel()
		t.Fatal("stale completion marked the new incarnation as notified")
	}
	e.close(t)

	restarted := fixture.open(t, config, func() time.Time { return now })
	if view := postgresOnlyCheck(t, restarted.Engine); view.Status != "awaiting" {
		t.Fatalf("stale completion was persisted to the new incarnation: %+v", view)
	}
	restarted.close(t)
	fixture.assertLegacySentinel(t)
}

func TestPostgresPausedStateSurvivesRestartBeforeUnpause(t *testing.T) {
	fixture := newPostgresFixture(t)
	id := postgresTestUUID(t)
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	activeConfig := postgresConfig(id, "24h", "1m", false, false, "")
	pausedConfig := postgresConfig(id, "24h", "1m", false, true, "")
	first := fixture.open(t, activeConfig, func() time.Time { return now })
	postgresPing(t, first.Engine, id, Input{Kind: "success"})
	if err := first.Reload(parseTestConfig(t, pausedConfig)); err != nil {
		t.Fatalf("pause check: %v", err)
	}
	first.close(t)

	now = now.Add(48 * time.Hour)
	second := fixture.open(t, pausedConfig, func() time.Time { return now })
	if view := postgresOnlyCheck(t, second.Engine); view.Status != "paused" {
		t.Fatalf("restart lost declared pause: %+v", view)
	}
	if err := second.Reload(parseTestConfig(t, activeConfig)); err != nil {
		t.Fatalf("unpause check: %v", err)
	}
	view := postgresOnlyCheck(t, second.Engine)
	if view.Status != "awaiting" || !view.LastSuccess.IsZero() || !view.NextDue.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("unpause did not start a fresh observation period: %+v", view)
	}
	second.close(t)
	fixture.assertLegacySentinel(t)
}

func TestPostgresEngineUsesSingleActiveLockAndPreservesOtherTables(t *testing.T) {
	fixture := newPostgresFixture(t)
	id := postgresTestUUID(t)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(id, "5m", "1m", false, false, "")
	first := fixture.open(t, config, func() time.Time { return now })
	if _, err := OpenEngine(context.Background(), parseTestConfig(t, config), func() time.Time { return now }, os.LookupEnv); err == nil {
		t.Fatal("opened a second engine while the session lock was held")
	}
	postgresPing(t, first.Engine, id, Input{Kind: "success"})
	first.close(t)
	second := fixture.open(t, config, func() time.Time { return now })
	if view := postgresOnlyCheck(t, second.Engine); view.Status != "up" {
		t.Fatalf("engine did not persist state after releasing the active lock: %+v", view)
	}
	second.close(t)
	fixture.assertLegacySentinel(t)
}
