# healthchecks-go

A small, self-hosted service for monitoring heartbeats and scheduled jobs.
One Go process, YAML configuration, PostgreSQL state, and bounded in-memory ping logs.

## MVP scope

- Healthchecks-compatible HTTP pings: success, start, failure, logs, exit codes, and run IDs.
- Interval and cron schedules with IANA time zones.
- Live configuration reload that preserves state without restarting the process.
- A read-only dashboard and a separate page for each check.
- One Prometheus metric family, `hc_check_up`, at `/metrics`.
- One optional HTTP webhook with a configurable JSON body.

The service runs as a single active instance. Check state and webhook progress survive restarts; recent ping logs do not.
It has no accounts, management API, or built-in regional inhibition.

## Configuration

[config.example.yml](config.example.yml) contains two fictional checks: an interval and a cron schedule.
Use `config.yml` for the runtime configuration. Keep existing UUIDs when migrating checks;
otherwise replace the explicit example UUID or omit `uuid` to derive it from the check name and a secret seed.
The default configuration path is `config.yml`; the default listen address is `:8000`.
Set `public_url` to an HTTP(S) origin, such as `https://status.example.org` or `http://localhost:8000`.
A trailing `/` is accepted; subpaths such as `/base` are rejected.

## Run

Requires Go 1.27 or later:

```sh
go build -o /tmp/healthchecks-go ./cmd/healthchecks-go
cp config.example.yml config.yml
export HC_UUID_SEED="$(cat /proc/sys/kernel/random/uuid)"
/tmp/healthchecks-go -config config.yml -check
/tmp/healthchecks-go -config config.yml -list-ping-urls
DB_HOST=localhost DB_NAME=healthchecks_go DB_USER=healthchecks DB_PASSWORD=local-password \
  DB_SSLMODE=disable /tmp/healthchecks-go -config config.yml
```

Generate `HC_UUID_SEED` once, save it in your deployment's secret configuration, and reuse it on every start.
The example above creates a new seed; running that line again changes all automatically generated UUIDs.
Open `http://localhost:8000/`. Send a success ping to `/ping/<uuid>` from the job being monitored.
`-check` and `-list-ping-urls` validate YAML and its environment references without connecting to PostgreSQL.
Database settings are required only when starting the server.
Keep deployment configuration and credentials out of a public repository.

### Container

After CI succeeds on `main`, GitHub Actions publishes `ghcr.io/zewelor/healthchecks-go:latest`
for Linux amd64 and arm64. The same image is tagged `sha-<full-commit-sha>`; the latest
ten tagged images are retained. The workflow summary records the published digest.
Pull requests only run verification. Use an image digest to pin a deployment.

To build and run locally:

```sh
docker build -t healthchecks-go:local .
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 127.0.0.1:8000:8000 -e HC_UUID_SEED \
  -e DB_HOST -e DB_PORT -e DB_NAME -e DB_USER -e DB_PASSWORD -e DB_SSLMODE \
  -v "$PWD/config-directory:/config:ro" healthchecks-go:local
```

Place `config.yml` in the mounted directory, readable by UID 65532. Mount the directory to allow atomic file replacement.
For Kubernetes ConfigMaps, use a directory mount without `subPath` and do not restart the Pod on configuration changes.
Use one replica with a strategy that prevents overlapping independent instances, such as `Recreate`.

### PostgreSQL

The supported target is PostgreSQL 18 or later; integration tests run on PostgreSQL 18.
SQL can use features available in 18 without compatibility workarounds for older versions.
The current state model needs JSONB, transactions, upserts and advisory locks; it does not require extensions.

The database and login role must already exist. Configure them with separate environment variables, so Kubernetes can reuse existing Secrets:

| Variable | Meaning | Default |
| --- | --- | --- |
| `DB_HOST` | PostgreSQL hostname | required |
| `DB_PORT` | PostgreSQL port | `5432` |
| `DB_NAME` | Existing database name | required |
| `DB_USER` | Login role | required |
| `DB_PASSWORD` | Login password | required |
| `DB_SSLMODE` | `require`, `verify-ca`, `verify-full`, or `disable` | `require` |

For example, set `DB_NAME=healthchecks_go`, `DB_USER=healthchecks`, and source `DB_PASSWORD` through a namespace-local `secretKeyRef`.
Use a direct PostgreSQL read/write endpoint, not a transaction-mode connection pooler.
`require` encrypts the connection; use `verify-full` and a trusted CA when server identity verification is required.
For a private endpoint without TLS, explicitly set `DB_SSLMODE=disable`.

Use a dedicated database, for example `healthchecks_go`. Startup creates two tables in the standard `public` schema:
`public.check_state` and `public.runtime`. The role must own this database (or have permission to create and use these tables).
Queries qualify `public` explicitly; there is no custom schema parameter or change to database/role defaults.
Keep the Python application's existing `healthchecks` database for rollback. The two applications can reuse the existing
`healthchecks` role and password, but sharing the role does not provide access isolation between their databases.
Keep this role and its credentials when cleaning up the Python installation. Backups must include the new database.
An unsupported schema version stops startup; schema changes and application rollback need matching versions.
There is no automatic import of Python runtime state: the first Go startup begins a new observation period.

A session advisory lock allows only one active instance for this database. Use one replica and `Recreate`.
The application reconnects after a database failure, reacquires the lock and reloads committed state before accepting changes.
This release does not support multiple active workers. Confirm network access to PostgreSQL before deployment.

## Configuration reference

Each check requires `name`, `grace`, and exactly one of `every` or `cron`.
Durations use Go syntax, such as `5m`, `1h30m`, and `24h`. Cron time zones default to UTC; intervals measure elapsed time.
Use `UTC` or an explicit IANA time zone such as `Europe/London`. Host-dependent `Local` is rejected.
Optional fields are `tags`, `region`, `depends_on_regions`, `paused`, and `notify`.
Tags are space-separated text. Region dependencies are metadata for external alert routing.

Cron uses a deliberately small five-field subset: minute, hour, day-of-month, month, day-of-week.
Each field accepts `*` or a single number. Minutes and hours also accept `*/N`, with steps from 1 to 60 and 1 to 24 respectively.
Sunday is `0` or `7`. Lists, ranges, names, macros, seconds, and extensions such as `L`, `W`, `#`, and `?` are rejected.
At least one of day-of-month and day-of-week must be `*`.

```text
*/15 * * * *   # Every 15 minutes
0 2 * * *      # Daily at 02:00
0 0 1 1 *      # January 1 each year
```

The schedule must have an occurrence in every calendar year in its configured time zone.
February 29 schedules and annual wildcard windows skipped by DST are rejected.
Unsupported syntax is a configuration error at startup or reload; it is never silently reinterpreted.

Cron time zones use robfig/cron with one fall-back rule: schedules with a fixed minute and hour
expect only the first occurrence of a repeated local time. For example, `30 2 * * *` expects
one ping at 02:30; after that, the next daily occurrence is tomorrow, even if 02:30 repeats today.
Schedules with a wildcard in the minute or hour field keep the library's repeated-time behavior.
Nonexistent spring-forward times are skipped. Half-hour offset changes can skip valid occurrences.
A skipped occurrence can delay failure detection. No scheduler-specific DST compatibility is guaranteed.
For schedules independent of clock changes, configure both the job and check in UTC.
Use `every` for elapsed-time intervals.

Metrics are available anonymously by default. To require a Bearer token, set the optional
environment variable `HC_METRICS_KEY` to a nonempty value. An unset or empty value allows anonymous access.
For containers, pass the variable with `-e HC_METRICS_KEY`.
The dashboard should be protected by an authentication proxy when needed; ping URLs authenticate by check UUID.

### Check identity

`uuid` is optional. When omitted or empty, `HC_UUID_SEED` must contain at least 32 bytes.
It is not needed when all checks have explicit UUIDs. The seed must be random, secret, and stable across restarts.
On Linux, `cat /proc/sys/kernel/random/uuid` generates a suitable value.

Generated IDs use HMAC-SHA256 keyed by the seed over `healthchecks-go/check/` followed by the exact UTF-8 name.
The first 16 digest bytes are formatted as [UUIDv8](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.8),
with the standard version and variant bits. This algorithm is part of the ping URL compatibility contract.
Case and whitespace in the name matter; schedule, tags, region, list order, and `public_url` do not affect the UUID.

Changing the name or seed creates a new identity, ping URL, and runtime state.
To rename a check while keeping its identity, first copy its generated UUID into `uuid`.
Two checks without explicit UUIDs cannot share a name; different explicit UUIDs may share a display name.
All resolved UUIDs are checked for conflicts before configuration is applied.

Use `-list-ping-urls` to print names and resolved ping URLs without starting a server.
It requires the same configuration and seed as the running service. Treat its output as credentials;
UUIDs are not exposed in dashboard pages, metrics, or webhook payloads automatically.

### Optional webhook

The webhook requires a display name and URL. `${VARIABLE}` expansion is supported only in `webhook.url`.
Missing or empty environment variables are configuration errors.
Set `notify: true` on checks that should send direct webhook notifications.

For example, a Discord webhook can use:

```yaml
webhook:
  name: discord
  url: "${DISCORD_WEBHOOK_URL}"
  body: |
    {"content": {{ json .message }}, "allowed_mentions": {"parse": []}}
```

Set `DISCORD_WEBHOOK_URL` to the webhook URL with `?wait=true`.
The `json` template function encodes values safely. Omitting `body` selects the default event JSON.
The name is a label; delivery always uses HTTP POST with `Content-Type: application/json`.
Changing the name or body at the same URL preserves delivered-outage state, so it neither repeats an existing down nor loses its recovery.
Editing the body also allows failed deliveries to retry with the corrected payload. Changing the URL starts delivery tracking for the new recipient.
Explicit ports in `public_url` and `webhook.url` must be between 1 and 65535; an omitted port uses the scheme's default.

YAML changes reload without losing state. Environment variable changes require a process restart.
Reload is checked every five seconds. Invalid updates and failed database transactions leave the previous configuration active and show a dashboard error.
A transient database failure retries the same YAML after recovery. Successful reload deletes removed checks and their pending notifications atomically.
Removing and re-adding a UUID starts fresh state; an old HTTP completion cannot alter the new check.
Tags and metadata can change without clearing check history. Names can change without resetting history when `uuid` is explicit.
Changing metric labels can still reset external alerts.

## Ping and state semantics

GET, HEAD, and POST support `/ping/<uuid>`, `/start`, `/fail`, `/log`, and exit codes `/0` through `/255`.
HEAD records a ping. `/start` does not record success, and `/log` does not refresh the success deadline.
Optional `rid=<uuid>` correlates a start and its completion. UUIDs and run IDs must use lowercase canonical form.
The response advertises `Ping-Body-Limit: 8192`. Only the first 8192 body bytes are stored; one additional byte is read to detect truncation.
The ping takes effect after that prefix is read; the service does not consume an arbitrarily large remaining upload.
Clients uploading very large bodies may see a closed connection even though the ping has taken effect.
Senders should respect `Ping-Body-Limit` and truncate logs before uploading them.
History is limited to 20 events per check, with at most 128 configured checks.

A newly configured check awaits its first signal until one interval plus grace, or the next cron occurrence plus grace.
Explicit failure acts immediately. Grace also limits how long a started execution may run.
`paused: true` keeps a check paused even if it receives pings. Unpausing starts a new observation period.
Restart restores success deadlines, active runs, explicit failures and notification progress, then applies the current YAML.
Ping bodies/history are process-local and reset on restart. A successful ping response is sent only after the state transaction commits.
Database failures return `503`; clients should retry. A connection loss during commit can make the result uncertain, so a retried ping may be recorded twice.

`/metrics` exports `hc_check_up` with
`name`, `tags`, `unique_key`, `region`, and `depends_on_regions` labels. Only `down` maps to 0; other states map to 1.
When `HC_METRICS_KEY` is nonempty, requests require `Authorization: Bearer <HC_METRICS_KEY>`.
Region dependencies are sorted and comma-separated. They do not suppress state or direct webhook notifications;
use external alert routing for inhibition and maintenance silences.
`/api/v3/status/` checks PostgreSQL availability and returns `503` during a database outage, independently of webhook delivery.
Use `/healthz` for process-only liveness, so a database outage does not cause restart loops.
The dashboard retains the last committed view and shows a database error when it cannot refresh it.
`/metrics` returns `503` instead of publishing stale check states when PostgreSQL cannot be reached.

Webhooks notify on down and recover after a successfully reported outage. Delivery runs independently of ping handling,
retries temporary failures and HTTP 429/5xx, and exposes persistent errors on the check page.
Pending recovery remains eligible during grace; a new down state supersedes an undelivered recovery.
A valid `Retry-After` pauses all deliveries to the shared webhook until the requested time, even when longer than five minutes.
The receiver's pause survives removal of the originating check after its HTTP response has completed.
Without it, exponential backoff is capped at five minutes per check. Changing the webhook URL clears the previous receiver's pause.
Retry deadlines and pending notifications survive restart. Each HTTP attempt is recorded before sending, with its exact payload.
An attempt with an uncertain outcome is replayed after restart, even if the check recovered meanwhile, then recovery follows a successful down.
A timeout or crash after recipient acceptance may produce a duplicate message. There is no exactly-once guarantee or separate queue.

## Development and compatibility

```sh
go vet ./...
go test -race ./...
bash scripts/show-build-context.sh
```

Tests include HTTP lifecycle scenarios, state/deadline boundaries, reload and ConfigMap symlink replacement,
webhook retries and in-flight recovery, bounded history, and HTML escaping.
See [the isolated compatibility harness](tests/compatibility/README.md) for comparison with pinned Healthchecks v4.4,
including the shared cron cases. DST differences are documented in the harness.
This is a defined Ping API subset, not a full Healthchecks API replacement.

PostgreSQL integration tests require an explicitly disposable, local database named `hc_test`.
They reset only `public.check_state` and `public.runtime` and create a sentinel fixture in `public`:

```sh
HC_TEST_POSTGRES=1 DB_HOST=127.0.0.1 DB_PORT=15432 DB_NAME=hc_test \
  DB_USER=hc_test DB_PASSWORD=local-test DB_SSLMODE=disable go test -race ./...
```

These tests exercise real PostgreSQL transactions and network interruptions, restart/replay,
configuration removal and retry, and rejection of a second active instance.
