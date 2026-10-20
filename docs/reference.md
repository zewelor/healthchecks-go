# Configuration and behavior reference

[Back to README](../README.md)

## Configuration

Each check requires a unique lowercase canonical `uuid`, `name`, `grace`, and exactly one of `every` or `cron`.
Generate a UUID with `cat /proc/sys/kernel/random/uuid` on Linux. Changing a check's name or metadata preserves identity;
changing its UUID starts a new check. At most 128 checks can be configured.

Durations use Go syntax, such as `5m`, `1h30m`, or `24h`. Intervals measure elapsed time.
Cron time zones default to UTC; use `UTC` or an IANA zone such as `Europe/London`. Host-dependent `Local` is rejected.
Tags are space-separated text. `depends_on_regions` is metadata for external alert routing;
it does not suppress state changes or direct webhook notifications.

`public_url` must be an HTTP(S) origin with no subpath, user information, query, or fragment. A trailing `/` is accepted.
Defaults: configuration file `config.yml`, listen address `:8000`. Override them with `-config` and `-listen`.
`-check` and `-list-ping-urls` validate configuration without connecting to PostgreSQL.

YAML is checked every five seconds. Invalid reloads leave the previous configuration active and show a dashboard error.
Removing a UUID deletes its state and pending notifications; re-adding it starts fresh.
Environment changes require a restart.

### Cron syntax

Five fields: minute, hour, day-of-month, month, day-of-week.
Each accepts `*` or a single number. Minutes and hours also accept `*/N`, with steps up to 60 and 24 respectively.
Sunday is `0` or `7`; at least one of day-of-month and day-of-week must be `*`.
Lists, ranges, names, macros, seconds, and extensions such as `L`, `W`, `#`, and `?` are rejected.

```text
*/15 * * * *   # Every 15 minutes
0 2 * * *      # Daily at 02:00
0 0 1 1 *      # January 1 each year
```

A schedule must have an occurrence every calendar year. February 29 schedules and annual windows skipped by DST are rejected.
Fixed minute-and-hour schedules use only the first occurrence of a repeated local time.
Wildcard schedules retain robfig/cron's repeated-time behavior. Nonexistent spring-forward times are skipped;
offset changes can delay failure detection. Use UTC to avoid clock changes, or `every` for elapsed-time intervals.

### Webhooks

Set `notify: true` on checks that should send notifications. The webhook needs a name and URL;
only `webhook.url` supports `${VARIABLE}` expansion, and missing or empty variables are rejected.
Omitting `body` uses the default event JSON. Custom bodies use Go templates with a `json` helper for safe JSON encoding.

For Discord:

```yaml
webhook:
  name: discord
  url: "${DISCORD_WEBHOOK_URL}"
  body: |
    {"content": {{ json .message }}, "allowed_mentions": {"parse": []}}
```

Use a Discord webhook URL ending in `?wait=true`. Delivery uses HTTP POST with `Content-Type: application/json`.
A down notification is followed by recovery after the outage was successfully reported.
Temporary failures and HTTP 429/5xx retry independently of ping handling. Persistent errors appear on the check page.
`Retry-After` pauses deliveries to the shared webhook; otherwise backoff is capped at five minutes.

Delivery progress and retry deadlines survive restart. Uncertain attempts can replay, so duplicate messages are possible.
Changing the name or body at the same URL preserves outage tracking; changing the URL starts tracking for the new recipient.

## Ping API

GET, HEAD, and POST support `/ping/<uuid>` and its suffixes:

| Suffix | Meaning |
| --- | --- |
| none or `/0` | Success |
| `/start` | Execution started |
| `/fail` or `/1`–`/255` | Failure |
| `/log` | Log only |

HEAD also records a ping. `/start` and `/log` do not refresh the success deadline.
Optional `rid=<uuid>` correlates a start with its completion; UUIDs and run IDs use lowercase canonical form.
Bodies are limited to 8192 bytes, advertised by `Ping-Body-Limit`; longer bodies are truncated.
The server does not consume an arbitrarily large upload, so clients should respect this limit.
History holds at most 20 events per check and resets on restart.

New checks await their first success until one interval plus grace, or the next cron occurrence plus grace.
Explicit failures act immediately. Grace also limits how long a started execution may run.
`paused: true` ignores pings for state changes; unpausing starts a new observation period.
Paused checks show no active due time in the dashboard. Other missed due times are shown as overdue.
Full timestamps appear on hover or keyboard focus in the browser's time zone.

Pings are acknowledged after the state transaction commits. Database failures return `503` and should be retried.
A connection loss during commit can leave the result uncertain; retrying may record the same ping twice.

## Storage and deployment

Use one active instance. PostgreSQL 18+ stores check state and webhook progress in `public.check_state` and `public.runtime`.
The database user needs permission to create and use these tables. A session advisory lock prevents concurrent instances.
Use a direct read/write endpoint rather than a transaction-mode connection pooler.
The application reconnects after database failures and reloads committed state before accepting changes.
Unsupported schema versions stop startup; there is no automatic import of Python Healthchecks runtime state.

`DB_SSLMODE` defaults to `require`. `verify-full` checks server identity with a trusted CA;
use `disable` only when the database endpoint intentionally has no TLS.

The container runs as UID 65532, so `config.yml` must be readable by that user.
Mount its directory to support atomic file replacement. For Kubernetes ConfigMaps, use a directory mount without `subPath`.
Use one replica and a strategy that prevents overlapping instances, such as `Recreate`.

CI publishes `ghcr.io/zewelor/healthchecks-go:latest` for Linux amd64 and arm64 after successful checks on `main`.
The image records the source commit in `org.opencontainers.image.revision`; the workflow summary includes its digest.

## Metrics and health endpoints

`/metrics` exports `hc_check_up` with `name`, `tags`, `unique_key`, and `depends_on_regions` labels.
Only `down` maps to 0; all other states map to 1. Region dependencies are sorted and comma-separated.
An unset or empty `HC_METRICS_KEY` allows anonymous access; otherwise requests need `Authorization: Bearer <HC_METRICS_KEY>`.
Ping UUIDs are credentials and are not exposed automatically in the dashboard, metrics, or webhook payloads.

- `/healthz`: process liveness, independent of PostgreSQL.
- `/api/v3/status/`: database readiness; returns `503` during an outage.
- `/metrics`: returns `503` rather than publishing stale state during a database outage.

The dashboard retains the last committed view and displays a database error when it cannot refresh.

## Testing

`go test -race ./...` covers HTTP lifecycle, state boundaries, reloads, webhook delivery, history limits, and HTML escaping.
The [compatibility harness](../tests/compatibility/README.md) compares the supported Ping API subset with Healthchecks v4.4.

PostgreSQL integration tests require an explicitly disposable local database named `hc_test`:

```sh
HC_TEST_POSTGRES=1 DB_HOST=127.0.0.1 DB_PORT=15432 DB_NAME=hc_test \
  DB_USER=hc_test DB_PASSWORD=local-test DB_SSLMODE=disable go test -race ./...
```

These tests reset `public.check_state` and `public.runtime` and create a sentinel fixture in `public`.
They cover transactions, outages, restart/replay, configuration removal, retry, and rejection of a second active instance.
Use `bash scripts/show-build-context.sh` to inspect the Docker build context.
