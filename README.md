# uptime-monitor

A lightweight uptime monitor written in Go. It periodically checks configured
websites, stores results in a **separate SQLite database per site**, and exposes
a **pull-only REST API** for other systems to query (uptime %, latency, errors,
incidents).

## Features

- Per-site config: URL, check interval, and each check type toggled on/off.
- Check types:
  - **http** — site responds with an acceptable status code.
  - **latency** — response time under a threshold.
  - **content** — body contains expected text / is free of error strings.
  - **ssl** — TLS certificate valid and not expiring soon (slow cadence).
  - **dns** — hostname resolves (slow cadence).
- Tracks uptime %, latency percentiles (p50/p95/p99), incidents (downtime
  periods), and a full error log.
- One SQLite file per site; lean one-row-per-tick schema
  (see [docs/STORAGE.md](docs/STORAGE.md)).

## Requirements

- Go 1.22+ (uses the stdlib router's method+path patterns).
- No cgo/gcc needed — uses the pure-Go `modernc.org/sqlite` driver.

## Build & run

```sh
go mod tidy        # fetch dependencies (first run)
go build -o monitor ./cmd/monitor
./monitor -data ./data -addr :8080
```

Or use the Makefile: `make build`, `make test`, `make run`.

### Deploying to Linux

The pure-Go SQLite driver means `CGO` can stay off, so the binary is fully
static and cross-compiles from any host (Windows/macOS/Linux) with no C
toolchain:

```sh
make linux          # -> static linux/amd64 binary
make linux-arm64    # -> static linux/arm64 (Graviton, Pi, etc.)
```

Copy the resulting `monitor` binary to the server — it has no runtime
dependencies. A minimal systemd unit:

```ini
[Unit]
Description=uptime-monitor
After=network-online.target

[Service]
ExecStart=/opt/uptime-monitor/monitor -data /var/lib/uptime-monitor -addr :8080
Environment=UPTIME_API_KEY=change-me
Restart=always

[Install]
WantedBy=multi-user.target
```

### Flags

| flag               | default   | purpose                                         |
|--------------------|-----------|-------------------------------------------------|
| `-data`            | `./data`  | directory for `registry.db` + per-site DBs      |
| `-addr`            | `:8080`   | REST API listen address                         |
| `-api-key`         | *(empty)* | require `X-API-Key` header (or `UPTIME_API_KEY`)|
| `-request-timeout` | `15s`     | per-check HTTP timeout                           |
| `-seed`            | *(empty)* | JSON file of site configs to import on startup  |
| `-block-private-targets` | `false` | refuse to check private/loopback/link-local addresses (SSRF guard) |
| `-cors-origins`    | `https://portal.pinkcrab.co.uk` | comma-separated browser origins allowed via CORS (empty = disabled) |

CORS is only relevant when a **browser** calls the API from another origin
(server-to-server clients ignore it). Listed origins are matched exactly and
echoed back in `Access-Control-Allow-Origin` — never `*`, since the API
authenticates with the `X-API-Key` header. Preflight `OPTIONS` requests are
answered with `204`. Set `-cors-origins=""` to disable CORS entirely.

Seed on first run to bootstrap sites:

```sh
./monitor -seed seed.example.json
```

## REST API

All data endpoints are read-only. Authorization (via the `X-API-Key` header)
has two tiers:

- **Admin key** — the global `-api-key` / `UPTIME_API_KEY`. Required for all
  config CRUD, and can read any site's data.
- **Per-site read key** — an optional key attached to a single site. It grants
  read access to *that site's* data endpoints only. Each consuming system holds
  just its own site's key.

`/health` is always unauthenticated. If no admin key is set and a site has no
key, that site's data endpoints are open (single-node default).

### Config

Create/update bodies may include a per-site key:
- `"api_key": "<value>"` — set the site's read key to a value you supply.
- `"generate_api_key": true` — have the server generate a random key; it is
  returned **once** as `api_key` in the response and only its SHA-256 is stored.
- `"api_key": ""` — clear the site's key. Omitting both preserves the existing
  key across updates.

Site config responses never include the key or its hash — only a `has_api_key`
boolean.


| method | path           | purpose                       |
|--------|----------------|-------------------------------|
| GET    | `/sites`       | list site configs             |
| POST   | `/sites`       | create a site                 |
| GET    | `/sites/{id}`  | get one site config           |
| PUT    | `/sites/{id}`  | update a site (id immutable)  |
| DELETE | `/sites/{id}`  | remove a site (`?purge=true` also deletes its data file) |

### Data (pull-only)

| method | path                        | purpose                                   |
|--------|-----------------------------|-------------------------------------------|
| GET    | `/sites/{id}/status`        | current up/down, last check, SSL expiry   |
| GET    | `/sites/{id}/uptime`        | averaged uptime % (`?window=1h\|24h\|7d\|30d\|all`) |
| GET    | `/sites/{id}/metrics`       | counts + latency avg/p50/p95/p99 (`?window=`) |
| GET    | `/sites/{id}/errors`        | stored errors (`?since=&limit=`)          |
| GET    | `/sites/{id}/incidents`     | downtime periods (`?limit=`)              |
| GET    | `/sites/{id}/results`       | raw check rows (`?since=&limit=`)         |
| GET    | `/health`                   | monitor liveness                          |

`window` defaults to `all` (since monitoring began). `since` is a unix
timestamp.

### Examples

```sh
# Add a site with a server-generated per-site key (admin key required for CRUD).
# The response includes "api_key": "<token>" exactly once — save it.
curl -X POST localhost:8080/sites \
  -H "X-API-Key: $ADMIN_KEY" -d '{
  "name": "My API",
  "url": "https://api.example.com/health",
  "interval_seconds": 60,
  "generate_api_key": true,
  "checks": {
    "http": {"enabled": true},
    "latency": {"enabled": true, "max_ms": 1500},
    "content": {"enabled": true, "must_not_contain": ["Exception"]},
    "ssl": {"enabled": true, "warn_days": 14},
    "dns": {"enabled": true}
  }
}'

# The consuming system reads its own site with the per-site key:
curl -H "X-API-Key: $MY_API_SITE_KEY" localhost:8080/sites/my-api/uptime
curl -H "X-API-Key: $MY_API_SITE_KEY" 'localhost:8080/sites/my-api/metrics?window=24h'

# Rotate a site's key (old key stops working immediately):
curl -X PUT localhost:8080/sites/my-api \
  -H "X-API-Key: $ADMIN_KEY" \
  -d '{"name":"My API","url":"https://api.example.com/health","generate_api_key":true}'
```

## Security

This service issues HTTP requests to operator-configured URLs and exposes a
management API. Before exposing it beyond localhost:

- **Set an admin API key** (`-api-key` / `UPTIME_API_KEY`). Config CRUD then
  requires it via the `X-API-Key` header (compared in constant time). Give each
  consumer a **per-site key** instead of the admin key so a leak is contained to
  one site. Per-site keys are stored as SHA-256 hashes, never in plaintext.
- **Terminate TLS at a reverse proxy** (nginx/Caddy). The API speaks plain HTTP,
  so the key travels in cleartext without one. Never expose it directly to an
  untrusted network.
- **SSRF:** creating a site makes the server fetch that URL, including on
  redirects. On a cloud host that can reach internal services and the metadata
  endpoint (`169.254.169.254`). If you do **not** need to monitor internal hosts,
  run with `-block-private-targets` to refuse dials to private/loopback/
  link-local addresses (checked against the resolved IP, so redirects and DNS
  rebinding are covered too). Leave it off only when internal monitoring is
  intended, and keep the API authenticated.
- **Site ids** are restricted to slugs and validated before touching the
  filesystem, so they cannot escape the data directory.
- **Request bodies** are capped at 1 MiB. **Credentials embedded in a monitored
  URL** (`https://user:pass@host`) are moved into an `Authorization` header so
  they never appear in logs or stored error messages.

See [docs/DEPLOY.md](docs/DEPLOY.md) for a production deployment runbook.

Error responses on `5xx` include internal error text to aid debugging; keep the
API behind authentication so that detail isn't exposed publicly.

## Layout

```
cmd/monitor/       daemon entrypoint (flags, wiring, shutdown)
internal/
  config/          site config model + validation
  storage/         registry DB, per-site DB manager, schema, queries
  checker/         one HTTP request per tick -> check evaluation
  scheduler/       one goroutine per site at its interval
  api/             REST handlers + router + auth
docs/STORAGE.md    schema & sizing rationale
data/              registry.db + per-site *.db (gitignored)
```
