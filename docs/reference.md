# Reference

[Back to README](../README.md)

## Configuration

UUIDs must be unique and use lowercase canonical form. Removing a check deletes its state.
Intervals measure elapsed time. Cron defaults to `UTC` and accepts IANA zones such as `Europe/London`; `Local` is rejected.
`depends_on_regions` is metadata for external alert routing, not built-in inhibition.

`description` is optional plain text, including multiple lines with YAML `|`. HTML and Markdown are not rendered.
It appears on hover or keyboard focus over the check name and below the name on its detail page.
Changing it reloads without resetting check state or ping history.

`public_url` accepts an HTTP(S) origin without a subpath, credentials, query, or fragment.
CLI overrides: `-config` (default `config.yml`) and `-listen` (default `:8000`).
Invalid YAML reloads keep the previous configuration active. Environment changes require a restart.

## Cron

Five fields: minute, hour, day-of-month, month, day-of-week.
Each accepts `*` or a single number; minutes and hours also accept `*/N` (maximum step 60 and 24 respectively).
Sunday is `0` or `7`. At least one of day-of-month and day-of-week must be `*`. Other syntax is rejected.

```text
*/15 * * * *   # Every 15 minutes
0 2 * * *      # Daily at 02:00
```

Schedules must occur every calendar year; February 29 schedules are rejected.
During DST changes, fixed minute/hour schedules use only the first occurrence of a repeated time;
wildcard schedules can run in both occurrences. Nonexistent local times are skipped.

## Webhooks

Set `notify: true` on the checks to notify on failure and recovery.
The webhook requires `name` and `url`. Only `webhook.url` supports `${VARIABLE}` expansion;
missing or empty variables are rejected. Omit `body` for default JSON, or use a Go template with the `json` helper.

Discord example:

```yaml
webhook:
  name: discord
  url: "${DISCORD_WEBHOOK_URL}"
  body: |
    {"content": {{ json .message }}, "allowed_mentions": {"parse": []}}
```

Use a Discord URL ending in `?wait=true`. Temporary delivery errors retry automatically;
persistent errors appear on the check page. Timeouts or restarts can result in duplicate notifications.

## Ping API

GET, HEAD, and POST support `/ping/<uuid>` with these suffixes:

| Suffix | Meaning |
| --- | --- |
| none or `/0` | Success |
| `/start` | Execution started |
| `/fail` or `/1`–`/255` | Failure |
| `/log` | Log only |

HEAD records a ping too. `/start` and `/log` do not refresh the success deadline.
Use `rid=<uuid>` to correlate start and completion; run IDs must be lowercase canonical UUIDs.
Limits: 128 checks, 20 recent events per check, and 8192 bytes per body (`Ping-Body-Limit`). Longer bodies are truncated.
The check detail page shows these recent pings newest first, with execution duration and expandable logs/run IDs.
This is an in-memory ping history, cleared on restart; it does not record deadline transitions or webhook deliveries.
Reload the page manually to see new pings and updated statuses; no updates are fetched in the background.

Grace applies both to missed successes and unfinished executions. Explicit failures act immediately.
Paused checks ignore pings for state changes; unpausing starts a new observation period.
Database failures return `503`; retrying an uncertain response can record a ping twice.

## Metrics and health

`hc_check_up` has `name`, `unique_key`, and `depends_on_regions` labels.
Only `down` maps to 0; other states map to 1.
With a nonempty `HC_METRICS_KEY`, `/metrics` requires `Authorization: Bearer <HC_METRICS_KEY>`.

- `/healthz`: process liveness, independent of PostgreSQL.
- `/api/v3/status/`: database readiness; returns `503` during an outage.
- `/metrics`: returns `503` during a database outage.

## Runtime

Application logs use structured text on stderr. `HC_LOG_LEVEL` sets the minimum severity:
`debug`, `info` (default), `warn`, or `error` (case-insensitive). Invalid values prevent startup.
Changes require a restart.

Accepted pings emit `INFO` after the state transaction succeeds, including the check name,
dashboard key, event kind, resulting status, whether the check is paused (`ignored`), and exit code when supplied.
Ping UUIDs, URLs, run IDs, request bodies, and authorization headers are omitted.
Successful configuration reloads emit `WARN`; reload, storage, and server errors emit `ERROR`.
Set `HC_LOG_LEVEL=warn` to silence ping logs while retaining reloads and errors.

PostgreSQL needs a persistent session; transaction-mode connection poolers are unsupported.
The container reads configuration as UID 65532. Mount the configuration directory for reloads;
Kubernetes ConfigMap mounts must not use `subPath`.

## Testing

The browser regression test requires `agent-browser` with Chromium installed:

```sh
HC_TEST_BROWSER=1 go test ./tests/e2e -run TestDashboardManualRefreshBrowser -v
```

It exercises the real HTTP handler and Content Security Policy, driving interval
callbacks manually. It verifies that statuses and history change only after a manual
reload, with no background requests, and covers saved preferences, tooltips, mobile
card navigation, escaped logs, and configuration changes. The default Go suite skips
this opt-in browser test.

PostgreSQL integration tests require a disposable local database named `hc_test`; they reset application data:

```sh
HC_TEST_POSTGRES=1 DB_HOST=127.0.0.1 DB_PORT=15432 DB_NAME=hc_test \
  DB_USER=hc_test DB_PASSWORD=local-test DB_SSLMODE=disable go test -race ./...
```

See the [compatibility harness](../tests/compatibility/README.md) for Ping API and cron comparisons with upstream Healthchecks.
