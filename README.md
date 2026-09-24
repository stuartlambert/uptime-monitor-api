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
has two tiers, and admin access accepts two kinds of credential.

- **Admin** — either the global `-api-key` / `UPTIME_API_KEY` (for machine
  consumers) or an **admin session cookie** obtained by logging in (for the web
  UI). Required for all config CRUD; either one can read any site's data.
- **Per-site read key** — an optional key attached to a single site. It grants
  read access to *that site's* data endpoints only. Each consuming system holds
  just its own site's key.

`/api/health` and `POST /api/auth/login` are the only unauthenticated endpoints.

**Authentication fails closed.** Earlier versions treated "no `-api-key` set" as
"auth disabled" and served config CRUD — including `DELETE` — to anyone who could
reach the port. That is gone: the daemon now **refuses to start** unless an admin
key or an admin account exists, and a site with no per-site key is admin-only
rather than public.

### Admin login

The first account is seeded once from `UPTIME_ADMIN_USER` and
`UPTIME_ADMIN_PASSWORD` on a boot where no account exists. After that the stored
hash is authoritative and those variables are ignored — remove them from the env
file once you have logged in.

To set or reset a password (also creates the account if missing), which prompts
on the terminal so nothing lands in shell history:

```sh
/opt/uptime-monitor/monitor -data /var/lib/uptime-monitor -set-password stuart
```

| method | path                  | purpose                                  |
|--------|-----------------------|------------------------------------------|
| POST   | `/api/auth/login`     | exchange username + password for a session cookie |
| POST   | `/api/auth/logout`    | revoke the current session server-side    |
| GET    | `/api/auth/me`        | who the caller is                         |
| POST   | `/api/auth/password`  | change password; revokes every session    |

Passwords are bcrypt hashed (cost 12) and must be at least 12 characters. The
session cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, valid for 7 days with a
sliding expiry; only a SHA-256 of it is stored, so a database leak yields no
usable sessions.

Login is throttled to 5 failures per username and per IP address in a 15-minute
window, after which it returns `429`. The counter is in memory, so restarting the
service clears a lockout.

### Identifying the client behind a proxy

The address used for rate-limit keys and log lines comes from `RemoteAddr` by
default, so no header a client sends can influence it. Behind a reverse proxy
that is always the proxy, so name the header the proxy sets:

```sh
monitor -real-ip-header=CF-Connecting-IP
```

Do **not** set this to `X-Forwarded-For` behind Cloudflare. Cloudflare appends
to any inbound `X-Forwarded-For` rather than replacing it, so the leftmost value
is whatever the caller sent — and since this value keys the login throttle,
trusting it would let an attacker rotate fabricated addresses past the per-IP
limit. `CF-Connecting-IP` is overwritten on every proxied request.

This holds only for traffic that actually reaches you through the proxy. If the
origin is also reachable directly, restrict it to the proxy's address ranges.

Password changes require a logged-in session and the current password — an API
key cannot change the password, so a leaked machine key cannot be escalated into
control of the account.

## Dashboard endpoints

Two rollups exist because the per-site endpoints are the wrong shape for a UI.

### `GET /api/sites/overview`

One row per site: its config, current up/down, last check, open incident, cert
expiry, and uptime over `?window=`. Without it, rendering ten sites means
twenty-one requests — and the rows would come from different instants, so the
page could show a site as both up and 100% down. **Admin only**: it spans every
site, so a per-site read key must not reach it.

```json
[{"site":{"id":"my-api", ...},"up":true,"last_check_ts":1787919436,
  "ongoing_incident_id":null,"window":"24h","checks":1440,"failed":3,
  "uptime_percent":99.79,"avg_ms":142.6}]
```

A site that has never been checked reports `"up": null` rather than `false`, so
the UI can distinguish *not yet known* from *down*. If one site's database
cannot be read, that row carries an `error` field and the rest still render.

### `GET /api/sites/{id}/series`

Uptime and latency bucketed server-side for charting. Takes `?window=` and
`?buckets=` (default 96, capped at 1000).

```json
{"window":"30d","from":1785327436,"to":1787919436,"bucket_seconds":21600,
 "points":[{"ts":1785327436,"checks":360,"successful":359,
            "uptime_percent":99.72,"avg_ms":141.3,"p95_ms":210}]}
```

Charting from `/results` does not scale: 30 days at a 60-second interval is
about 43,000 rows per site, and `/results` caps at 10,000 — so it cannot return
the window at all. Measured on that dataset, `/series` answers in ~67ms with a
12KB payload against 750KB for a *truncated* `/results`.

Buckets are dense: a period with no checks is returned with `"checks": 0` rather
than omitted, so a gap in monitoring is visible in the chart instead of being
silently closed up. `p95_ms` is a true 95th percentile computed per bucket, so a
single slow outlier moves the maximum but not the line.

## Admin UI

A web interface lives in `web/` — a separate Vite + React project with its own
build, deployed as static files and served from the same origin as the API.

```sh
make web-install   # once
make web           # production build -> web/dist
make web-dev       # Vite dev server on :5173, proxying /api to :8080
```

It is deliberately **not** embedded in the Go binary: the two have different
release cadences, and keeping them apart means a UI change does not require
rebuilding and restarting the monitor.

Deployment is covered in `docs/ROLLOUT.md` §9–10: `web/dist` goes in the web
server's docroot, and the web server proxies `/api/` to `127.0.0.1:8080`. Because
both are served from one origin, the session cookie works with `SameSite=Lax`
and no CORS configuration is involved.

Authentication is the session from `POST /api/auth/login`; the cookie is
`HttpOnly`, so the page never holds a credential in JavaScript. There are no
external runtime requests — no CDN and no web fonts — so it works on a network
with no outbound access.

## Alerting

Alerts are delivered by email over SMTP. Nothing is sent until you create a
**channel** (a destination) and a **rule** (what triggers a send), so a fresh
install is silent by default.

Three kinds fire:

| kind | when |
|------|------|
| `site_down` | a site has failed `confirm_after` consecutive checks |
| `site_recovered` | a site that was announced down comes back |
| `ssl_expiring` | the certificate is within the site's `ssl.warn_days` |

There is no separate latency alert: a response over `latency.max_ms` is recorded
as a failed check, so it already opens an incident and fires `site_down`.

| method | path                              | purpose                    |
|--------|-----------------------------------|----------------------------|
| GET    | `/api/alerts/channels`            | list destinations          |
| POST   | `/api/alerts/channels`            | create one                 |
| PUT    | `/api/alerts/channels/{id}`       | update                     |
| DELETE | `/api/alerts/channels/{id}`       | delete (cascades to rules) |
| POST   | `/api/alerts/channels/{id}/test`  | send a test message now    |
| GET    | `/api/alerts/rules`               | list rules                 |
| POST   | `/api/alerts/rules`               | create one                 |
| PUT    | `/api/alerts/rules/{id}`          | update                     |
| DELETE | `/api/alerts/rules/{id}`          | delete                     |
| GET    | `/api/alerts/deliveries`          | send log (`?site_id=&limit=`) |

A rule with no `site_id` applies to every site, including ones added later.

### Behaviour worth knowing

- **Confirmation.** `confirm_after` (default 2) holds a down alert until that
  many consecutive failures, so one blip sends nothing.
- **One mail per incident.** Deduplication is a `UNIQUE` key claimed in the
  database before sending, so a restart mid-outage cannot re-announce it.
- **Recovery is paired per channel.** A channel is only told a site recovered if
  that same channel was told it went down — so give a channel both rules, or its
  recovery rule never fires.
- **Certificate alerts key on the expiry date**, so an hourly check mails once,
  and a renewed certificate can alert again on its own terms.
- **A permanently failing channel stops after its retries.** The delivery row is
  kept as `failed`, which keeps the dedupe key claimed rather than retrying on
  every subsequent check.
- **Delivery never blocks monitoring.** Events go to a bounded queue handled by
  separate workers; if it saturates, events are dropped with a log line rather
  than stalling checks.

### SMTP configuration

Environment only — never flags (visible in `ps`) or the database:

```sh
UPTIME_SMTP_HOST=smtp.example.com
UPTIME_SMTP_PORT=587          # 465 = implicit TLS; anything else = STARTTLS
UPTIME_SMTP_USER=username
UPTIME_SMTP_PASS=password
UPTIME_SMTP_FROM=uptime@example.com
```

On any port but 465 the sender **requires** STARTTLS and refuses to continue
without it rather than sending credentials in clear. Without SMTP configured the
service still runs and still records deliveries; it just cannot send.

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
| GET    | `/api/sites`       | list site configs             |
| POST   | `/api/sites`       | create a site                 |
| GET    | `/api/sites/overview` | every site with its live state and uptime (admin only) |
| GET    | `/api/sites/{id}`  | get one site config           |
| PUT    | `/api/sites/{id}`  | update a site (id immutable)  |
| DELETE | `/api/sites/{id}`  | remove a site (`?purge=true` also deletes its data file) |

### Data (pull-only)

| method | path                        | purpose                                   |
|--------|-----------------------------|-------------------------------------------|
| GET    | `/api/sites/{id}/status`        | current up/down, last check, SSL expiry   |
| GET    | `/api/sites/{id}/uptime`        | averaged uptime % (`?window=1h\|24h\|7d\|30d\|all`) |
| GET    | `/api/sites/{id}/metrics`       | counts + latency avg/p50/p95/p99 (`?window=`) |
| GET    | `/api/sites/{id}/errors`        | stored errors (`?since=&limit=`)          |
| GET    | `/api/sites/{id}/incidents`     | downtime periods (`?limit=`)              |
| GET    | `/api/sites/{id}/results`       | raw check rows (`?since=&limit=`)         |
| GET    | `/api/sites/{id}/series`        | bucketed uptime + latency for charts (`?window=&buckets=`) |
| GET    | `/api/health`                   | monitor liveness                          |

### Path prefix and migration

The API is served under **`/api/`**. For a transition period it is *also* served
on the original unprefixed paths (`/sites`, `/health`, …) so existing consumers
keep working. Every request arriving on a legacy path logs a warning naming the
caller, rate-limited to one line per path per hour:

```
api: DEPRECATED legacy path GET /sites from 203.0.113.7 — migrate to /api/sites
```

Migrate your consumers, watch until those warnings stop, then drop the old
paths by adding `-legacy-routes=false` to the service's `ExecStart` line and
restarting. Both mounts share one router and one backend, so they can never
disagree; the only difference is the prefix.

`window` defaults to `all` (since monitoring began). `since` is a unix
timestamp.

### Examples

```sh
# Add a site with a server-generated per-site key (admin key required for CRUD).
# The response includes "api_key": "<token>" exactly once — save it.
curl -X POST localhost:8080/api/sites \
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
curl -H "X-API-Key: $MY_API_SITE_KEY" localhost:8080/api/sites/my-api/uptime
curl -H "X-API-Key: $MY_API_SITE_KEY" 'localhost:8080/api/sites/my-api/metrics?window=24h'

# Rotate a site's key (old key stops working immediately):
curl -X PUT localhost:8080/api/sites/my-api \
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
