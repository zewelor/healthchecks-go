# healthchecks-go

A small, self-hosted service for monitoring heartbeats and scheduled jobs.
YAML configuration, PostgreSQL persistence, and a minimal read-only dashboard.

[Get started](#get-started) · [Configuration](#configuration) · [Reference](docs/reference.md)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/dashboard-dark.jpg">
  <source media="(prefers-color-scheme: light)" srcset="docs/images/dashboard-light.jpg">
  <img src="docs/images/dashboard-light.jpg" alt="Healthchecks dashboard with six fictional checks, status filters, sorting, and light and dark themes.">
</picture>

- Interval and cron schedules with time zone support.
- Success, start, failure, and log pings compatible with the Healthchecks Ping API.
- Light and dark themes, status filters, sorting, and relative timestamps.
- Live configuration reload, Prometheus metrics, and optional webhooks.

## Get started

Requires PostgreSQL 18+ with an existing database and user. The application creates its tables on startup.
Set the database environment variables below, then:

```sh
cp config.example.yml config.yml
# Edit config.yml with your checks and public URL.
docker run --rm -p 127.0.0.1:8000:8000 \
  -e DB_HOST -e DB_PORT -e DB_NAME \
  -e DB_USER -e DB_PASSWORD -e DB_SSLMODE \
  -e HC_SITE_NAME -e HC_METRICS_KEY \
  -v "$PWD:/config:ro" ghcr.io/zewelor/healthchecks-go:latest
```

Open <http://localhost:8000/>. Run one instance; check state survives restarts, recent ping logs do not.

| Variable | Value |
| --- | --- |
| `DB_HOST` | PostgreSQL host reachable from the container |
| `DB_PORT` | Port, default `5432` |
| `DB_NAME` | Database name |
| `DB_USER` | Database user |
| `DB_PASSWORD` | Database password |
| `DB_SSLMODE` | `require` (default), `verify-ca`, `verify-full`, or `disable` |

## Configuration

Start with [config.example.yml](config.example.yml). Each check needs `uuid`, `name`, `grace`,
and either `every` (such as `5m` or `24h`) or `cron`. Keep UUIDs stable to preserve ping URLs and state.
Optional fields: `timezone`, `tags`, `paused`, `notify`, and `depends_on_regions`.
Set `public_url` to the service's HTTP(S) origin. YAML changes reload automatically.

Set `HC_SITE_NAME` to customize the dashboard title. Theme, filter, and sort preferences are saved in the browser.
For cron syntax, webhooks, and other details, see the [reference](docs/reference.md).

## Pings and metrics

Send a success ping to `/ping/<uuid>`. Append `/start`, `/fail`, `/log`, or an exit code `/0`–`/255` for other events.
Generate the configured URLs with `-list-ping-urls`; validate configuration with `-check`.

Prometheus metrics are available at `/metrics`. Set `HC_METRICS_KEY` to require a Bearer token.
The dashboard has no built-in login; use an authentication proxy if needed.

## Development

Requires Go 1.27+. Run from source with `go run ./cmd/healthchecks-go -config config.yml`
using the same database environment variables.

```sh
go vet ./...
go test -race ./...
```

For a dashboard demo without PostgreSQL, run `go run ./scripts/dashboard-preview` and open <http://127.0.0.1:18081/>.
See the [reference](docs/reference.md#testing) for PostgreSQL integration tests and compatibility checks.

Licensed under [MIT](LICENSE).
