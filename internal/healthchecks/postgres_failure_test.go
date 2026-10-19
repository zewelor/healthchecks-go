package healthchecks

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A real network boundary: drop existing connections and reject reconnects
// without changing PostgreSQL or any application configuration.
type databaseProxy struct {
	listener   net.Listener
	target     string
	mu         sync.Mutex
	blocked    bool
	clients    map[net.Conn]struct{}
	wg         sync.WaitGroup
	loseCommit atomic.Bool
}

func newDatabaseProxy(t *testing.T) *databaseProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &databaseProxy{listener: listener, target: net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT")), clients: make(map[net.Conn]struct{})}
	p.wg.Go(func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.blocked {
				p.mu.Unlock()
				client.Close()
				continue
			}
			p.clients[client] = struct{}{}
			p.wg.Go(func() {
				defer client.Close()
				defer func() { p.mu.Lock(); delete(p.clients, client); p.mu.Unlock() }()
				upstream, err := net.DialTimeout("tcp", p.target, time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				done := make(chan struct{})
				go func() { io.Copy(upstream, client); upstream.Close(); close(done) }()
				p.copyResponses(client, upstream)
				client.Close()
				<-done
			})
			p.mu.Unlock()
		}
	})
	t.Cleanup(func() { listener.Close(); p.block(true); p.wg.Wait() })
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	t.Setenv("DB_HOST", "127.0.0.1")
	t.Setenv("DB_PORT", port)
	return p
}

// Discard a server-confirmed COMMIT before the client receives CommandComplete.
// PostgreSQL's backend frames have a one-byte type, a length including its four
// bytes, and a payload; CommandComplete carries the zero-terminated command tag.
func (p *databaseProxy) copyResponses(client, upstream net.Conn) {
	for {
		var header [5]byte
		if _, err := io.ReadFull(upstream, header[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(header[1:])
		if size < 4 || size > 16<<20 {
			return
		}
		payload := make([]byte, int(size)-4)
		if _, err := io.ReadFull(upstream, payload); err != nil {
			return
		}
		if header[0] == 'C' && string(payload) == "COMMIT\x00" && p.loseCommit.CompareAndSwap(true, false) {
			return
		}
		if _, err := client.Write(header[:]); err != nil {
			return
		}
		if _, err := client.Write(payload); err != nil {
			return
		}
	}
}

func (p *databaseProxy) block(blocked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = blocked
	if blocked {
		for client := range p.clients {
			client.Close()
		}
	}
}

func requestStatus(e *Engine, path string) int {
	result := httptest.NewRecorder()
	NewHandler(e, "").ServeHTTP(result, httptest.NewRequest("GET", path, nil))
	return result.Code
}

func TestPostgresOutageRejectsPingsAndRetriesSameConfiguration(t *testing.T) {
	fixture := newPostgresFixture(t)
	proxy := newDatabaseProxy(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(testUUID, "24h", "1m", false, false, "")
	e := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, e.Engine, testUUID, Input{Kind: "fail"})
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	watcher := NewConfigWatcher(path, e.Engine, os.LookupEnv)
	if _, err := watcher.Check(); err != nil {
		t.Fatal(err)
	}
	proxy.block(true)
	if got := requestStatus(e.Engine, "/ping/"+testUUID); got != 503 {
		t.Fatalf("outage ping status=%d", got)
	}
	if got := requestStatus(e.Engine, "/api/v3/status/"); got != 503 {
		t.Fatalf("outage readiness=%d", got)
	}
	if got := requestStatus(e.Engine, "/metrics"); got != 503 {
		t.Fatalf("outage metrics published stale state: %d", got)
	}
	if got := requestStatus(e.Engine, "/healthz"); got != 200 {
		t.Fatalf("outage liveness=%d", got)
	}
	if got := postgresOnlyCheck(t, e.Engine); got.Status != "down" || len(got.Events) != 1 {
		t.Fatalf("rejected ping changed view: %+v", got)
	}
	replacementID := "22222222-2222-4222-8222-222222222222"
	replacement := strings.ReplaceAll(config, testUUID, replacementID)
	if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := watcher.Check(); err == nil || changed {
		t.Fatal("reload accepted during outage")
	}
	if e.Info().ReloadError == "" || e.Info().StorageError == "" {
		t.Fatal("database failure not visible")
	}
	proxy.block(false)
	if changed, err := watcher.Check(); err != nil || !changed {
		t.Fatalf("same YAML was not retried after recovery: changed=%v err=%v", changed, err)
	}
	if got := requestStatus(e.Engine, "/ping/"+testUUID); got != 404 {
		t.Fatalf("removed UUID accepted: %d", got)
	}
	if got := requestStatus(e.Engine, "/ping/"+replacementID); got != 200 {
		t.Fatalf("replacement ping failed: %d", got)
	}
	if got := requestStatus(e.Engine, "/api/v3/status/"); got != 200 {
		t.Fatalf("readiness did not recover: %d", got)
	}
	e.close(t)
	restarted := fixture.open(t, replacement, func() time.Time { return now })
	if got := postgresOnlyCheck(t, restarted.Engine); got.Status != "up" {
		t.Fatalf("recovered ping not durable: %+v", got)
	}
}

func TestPostgresRetriesAcknowledgementAfterDatabaseOutage(t *testing.T) {
	fixture := newPostgresFixture(t)
	proxy := newDatabaseProxy(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(testUUID, "24h", "1m", true, false, "https://example.test/hook")
	e := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, e.Engine, testUUID, Input{Kind: "fail"})
	d, _ := e.prepareDelivery(context.Background())
	if d == nil {
		t.Fatal("missing down delivery")
	}
	// Recovery can arrive while the down request is still being accepted.
	postgresPing(t, e.Engine, testUUID, Input{Kind: "success"})
	proxy.block(true)
	if err := e.finishDelivery(d, 204, "", nil); err == nil {
		t.Fatal("acknowledgement succeeded during database outage")
	}
	proxy.block(false)
	if err := e.finishDelivery(d, 204, "", nil); err != nil {
		t.Fatal(err)
	}
	recovery, _ := e.prepareDelivery(context.Background())
	if recovery == nil || recovery.status != "up" {
		t.Fatal("successful down acknowledgement lost recovery")
	}
	if err := e.finishDelivery(recovery, 204, "", nil); err != nil {
		t.Fatal(err)
	}
	e.close(t)
	restarted := fixture.open(t, config, func() time.Time { return now })
	if next, _ := restarted.prepareDelivery(context.Background()); next != nil {
		t.Fatal("acknowledged notification repeated after restart")
	}
}

func TestPostgresHTTPWorkerPreservesRecoveryAcrossOutage(t *testing.T) {
	fixture := newPostgresFixture(t)
	proxy := newDatabaseProxy(t)
	events := make(chan string, 4)
	downAccepted, releaseDown := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseDown) })
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event WebhookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		events <- event.Status
		if event.Status == "down" {
			close(downAccepted)
			<-releaseDown
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	defer release()
	config := postgresConfig(testUUID, "24h", "1m", true, false, receiver.URL)
	e := fixture.open(t, config, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.RunWebhook(ctx) }()
	defer func() { cancel(); <-done }()
	if got := requestStatus(e.Engine, "/ping/"+testUUID+"/fail"); got != 200 {
		t.Fatalf("failure ping=%d", got)
	}
	select {
	case <-downAccepted:
	case <-time.After(3 * time.Second):
		t.Fatal("down HTTP request not received")
	}
	if got := requestStatus(e.Engine, "/ping/"+testUUID); got != 200 {
		t.Fatalf("recovery ping=%d", got)
	}
	proxy.block(true)
	release()
	waitFor(t, "failed acknowledgement to be visible", func() bool { return e.Info().StorageError != "" })
	proxy.block(false)
	if got := receiveWebhookStatus(t, events); got != "down" {
		t.Fatalf("first HTTP notification=%s", got)
	}
	if got := receiveWebhookStatus(t, events); got != "up" {
		t.Fatalf("second HTTP notification=%s", got)
	}
	waitFor(t, "durable recovery acknowledgement", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		s := e.checks[testUUID]
		return !s.notifiedDown && s.pending == nil && s.flight == nil
	})
	cancel()
	<-done
	e.close(t)
	restarted := fixture.open(t, config, time.Now)
	if next, _ := restarted.prepareDelivery(context.Background()); next != nil {
		t.Fatal("completed HTTP lifecycle was not durable")
	}
}

func TestPostgresWriteFailureRollsBackPingAndConfigWithoutCancelingFlight(t *testing.T) {
	fixture := newPostgresFixture(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(testUUID, "24h", "1m", true, false, "https://example.test/hook")
	e := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, e.Engine, testUUID, Input{Kind: "fail"})
	d, _ := e.prepareDelivery(context.Background())
	if d == nil {
		t.Fatal("missing durable flight")
	}
	admin, err := pgx.Connect(context.Background(), fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	_, err = admin.Exec(context.Background(), `CREATE FUNCTION public.hc_test_reject_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'simulated disk write failure'; END $$;
CREATE TRIGGER hc_test_reject_write BEFORE INSERT OR UPDATE OR DELETE ON public.check_state FOR EACH ROW EXECUTE FUNCTION public.hc_test_reject_write()`)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP TRIGGER IF EXISTS hc_test_reject_write ON public.check_state; DROP FUNCTION IF EXISTS public.hc_test_reject_write()`)
	if _, err := e.Ping(testUUID, Input{Kind: "success"}); err == nil {
		t.Fatal("ping accepted despite failed write")
	}
	replacement := strings.ReplaceAll(config, testUUID, "22222222-2222-4222-8222-222222222222")
	if err := e.Reload(parseTestConfig(t, replacement)); err == nil {
		t.Fatal("reload accepted despite failed write")
	}
	if d.ctx.Err() != nil {
		t.Fatal("failed reload canceled an active delivery")
	}
	if got := postgresOnlyCheck(t, e.Engine); got.Status != "down" || len(got.Events) != 1 {
		t.Fatalf("failed transaction changed check: %+v", got)
	}
	if _, err = admin.Exec(context.Background(), `DROP TRIGGER hc_test_reject_write ON public.check_state; DROP FUNCTION public.hc_test_reject_write()`); err != nil {
		t.Fatal(err)
	}
	if err := e.finishDelivery(d, 204, "", nil); err != nil {
		t.Fatal(err)
	}
	e.close(t)
	restarted := fixture.open(t, config, func() time.Time { return now })
	if got := postgresOnlyCheck(t, restarted.Engine); got.Status != "down" {
		t.Fatalf("failed ping was persisted: %+v", got)
	}
	if next, _ := restarted.prepareDelivery(context.Background()); next != nil {
		t.Fatal("successful delivery was not recorded after write recovery")
	}
}

func TestPostgresRejectsUnknownSchemaVersion(t *testing.T) {
	fixture := newPostgresFixture(t)
	config := postgresConfig(testUUID, "24h", "1m", false, false, "")
	e := fixture.open(t, config, time.Now)
	postgresPing(t, e.Engine, testUUID, Input{Kind: "fail"})
	e.close(t)
	admin, err := pgx.Connect(context.Background(), fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(context.Background(), `UPDATE public.runtime SET version=2`); err != nil {
		t.Fatal(err)
	}
	failed, err := OpenEngine(context.Background(), parseTestConfig(t, config), time.Now, os.LookupEnv)
	if failed != nil {
		failed.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported database schema version") {
		t.Fatalf("unknown version accepted or hidden: %v", err)
	}
	if _, err := admin.Exec(context.Background(), `UPDATE public.runtime SET version=1`); err != nil {
		t.Fatal(err)
	}
	restarted := fixture.open(t, config, time.Now)
	if got := postgresOnlyCheck(t, restarted.Engine); got.Status != "down" {
		t.Fatal("version rejection changed state")
	}
}

func TestPostgresAcknowledgementRetryKeepsOriginalReceiverDeadline(t *testing.T) {
	fixture := newPostgresFixture(t)
	proxy := newDatabaseProxy(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(testUUID, "24h", "1m", true, false, "https://example.test/hook")
	e := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, e.Engine, testUUID, Input{Kind: "fail"})
	d, _ := e.prepareDelivery(context.Background())
	if d == nil {
		t.Fatal("missing down delivery")
	}
	proxy.block(true)
	if err := e.finishDelivery(d, 503, "60", nil); err == nil {
		t.Fatal("acknowledgement succeeded during database outage")
	}
	now = now.Add(5 * time.Minute)
	proxy.block(false)
	if err := e.finishDelivery(d, 503, "60", nil); err != nil {
		t.Fatal(err)
	}
	if next, wait := e.prepareDelivery(context.Background()); next == nil || wait != 0 {
		t.Fatalf("Retry-After restarted after acknowledgement retry: delivery=%v wait=%s", next != nil, wait)
	}
}

func TestPostgresReloadsCommittedPingAfterLostCommitResponse(t *testing.T) {
	fixture := newPostgresFixture(t)
	proxy := newDatabaseProxy(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	config := postgresConfig(testUUID, "24h", "1m", false, false, "")
	e := fixture.open(t, config, func() time.Time { return now })
	postgresPing(t, e.Engine, testUUID, Input{Kind: "fail"})
	proxy.loseCommit.Store(true)
	if _, err := e.Ping(testUUID, Input{Kind: "success"}); err == nil {
		t.Fatal("client did not observe lost COMMIT response")
	}
	if proxy.loseCommit.Load() {
		t.Fatal("proxy did not observe PostgreSQL COMMIT completion")
	}
	if got := postgresOnlyCheck(t, e.Engine); got.Status != "up" || !got.LastSuccess.Equal(now) {
		t.Fatalf("reconnect overwrote committed ping with old RAM: %+v", got)
	}
	e.close(t)
	restarted := fixture.open(t, config, func() time.Time { return now })
	if got := postgresOnlyCheck(t, restarted.Engine); got.Status != "up" || !got.LastSuccess.Equal(now) {
		t.Fatalf("uncertain commit did not remain durable: %+v", got)
	}
}
