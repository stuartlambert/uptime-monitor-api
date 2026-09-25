---
title: Features
updated: 2026-09-25
---

# Features

## Checks

Each site runs a fast cadence (default 60s) and a slow one (default hourly).

| Check | Cadence | Fails when |
|---|---|---|
| HTTP status | fast | status not in `expect_status`; if empty, anything outside 2xx/3xx |
| Latency | fast | slower than `max_ms`; `0` records timings without ever failing |
| Content | fast | body lacks a `must_contain` string, or holds a `must_not_contain` one |
| SSL | slow | certificate expired; warns within `warn_days` |
| DNS | slow | hostname does not resolve |

Any failing check marks the tick down and opens an incident, so a slow response
is an outage rather than a separate alert class.

| Setting | Default | Bounds |
|---|---|---|
| `interval_seconds` | 60 | 5 – 86400 |
| `slow_interval_seconds` | 3600 | ≥ `interval_seconds` |
| `ssl.warn_days` | 14 | — |
| `latency.max_ms` | 0 (off) | — |

Configured in the UI on the add/edit site form, or by API.

`-block-private-targets` is set in production, so private, loopback and
link-local addresses are refused — an SSRF guard. It hooks the dial after DNS
resolution, so it covers redirects and defeats DNS rebinding. Remove the flag to
monitor internal hosts.

**Code:** `internal/checker/checker.go` (`RunTick()`, `NewRunner()`, `blockPrivateAddr()`); scheduling in `internal/scheduler/scheduler.go` (`runSite()`)

## Stored history and statistics

Per site, in its own SQLite file: every check result, incidents with cause and
duration, and an error log. Uptime percentage and latency are computed at query
time, not stored.

Latency covers **successful checks only**, so a fast-failing 500 does not flatter
the average. Uptime and `avg_ms` are rounded to two decimal places.

Nothing is pruned — about 30 MB per site per year at one check a minute. See
`docs/STORAGE.md` in the repo.

**Code:** `internal/storage/query.go` (`Uptime()`, `Metrics()`, `Incidents()`, `Errors()`); write path in `internal/storage/tick.go` (`RecordTick()`)

## Admin UI

| Page | Path | What it shows |
|---|---|---|
| Dashboard | `/` | every site: status, uptime, average response, latency sparkline, last check, certificate expiry. Polls every 30s |
| Site detail | `/site/:id` | uptime bars, p95 latency chart, incidents, recent errors, danger zone |
| History | `/site/:id/history` | full paginated incidents and errors for one site |
| Add / edit | `/site/new`, `/site/:id/edit` | every check option, each revealed as its check is enabled |
| Alerts | `/alerts` | destinations, rules, delivery log, send-test |
| Settings | `/settings` | change password |
| Login | `/login` | the app's own login, behind Cloudflare Access |

Clicking a bar on the uptime graph filters the incidents list to that bar's
window. Both that and the history page work client-side over an already-fetched
set, so neither needs a backend call.

The UI lives under `/site/…`, deliberately not `/sites/…`, because `/sites/` is
the API's namespace and the two collided — a reload on a site page returned JSON.

**Code:** `web/src/App.tsx` (routes); pages in `web/src/pages/`; `web/src/api/client.ts` (fetch layer, 401 handling)

## Dashboard rollups

`/sites/overview` returns config, live state and uptime for every site in one
response — without it a ten-site dashboard is 21 requests, and the rows would
come from different instants. `/sites/{id}/series` buckets uptime and latency in
SQL: 30 days at a 60-second interval is ~43,000 rows, which `/results` caps out
on at 10,000.

A site never checked reports `"up": null` rather than `false`, so the UI can tell
*not yet known* from *down*. If one site's database cannot be read, that row
carries an `error` field and the rest still render.

**Code:** `internal/storage/overview.go` (`Overview()`), `internal/storage/series.go` (`Series()`, `fillSeriesP95()`); handlers `handleOverview()`, `handleSeries()` in `internal/api/api.go`

## Alerting

Email over SMTP. Nothing is sent until a **channel** (a destination) and a
**rule** (what triggers a send) exist.

| Kind | Fires when |
|---|---|
| `site_down` | a site has failed `confirm_after` consecutive checks |
| `site_recovered` | a site that was announced down comes back |
| `ssl_expiring` | the certificate is within that site's `warn_days` |

A rule with no `site_id` covers every site, including ones added later. There is
no latency alert: a slow response is already a failed check, so it fires
`site_down`.

Behaviour worth knowing:

- **Confirmation.** `confirm_after` (default 2) holds a down alert until that
  many consecutive failures, so a single blip sends nothing.
- **One mail per incident.** Deduplication is a `UNIQUE` key claimed in the
  database before sending, so a restart mid-outage cannot re-announce it.
- **Recovery is paired per channel.** A channel is only told a site recovered if
  that same channel was told it went down — so give a channel both rules, or its
  recovery rule never fires. The Alerts page warns when this is misconfigured.
- **Certificate alerts key on the expiry date**, so an hourly check mails once,
  and a renewed certificate alerts again on its own terms.
- **A permanently failing channel stops** after its retries; the row is kept as
  `failed`, which prevents a mail loop.
- **Delivery never blocks monitoring.** Events go to a bounded queue handled by
  separate workers; if it saturates, events are dropped with a log line rather
  than stalling checks.

Configured: channel "Pink Crab" → `hello@pinkcrab.co.uk`, with `site_down`
(confirm after 2), `site_recovered` and `ssl_expiring` rules for all sites.

**Code:** `internal/alerts/alerts.go` (`Dispatcher`, `handle()`, `applyRule()`, `render()`, `deliver()`), `internal/alerts/smtp.go` (`SMTPSender.Send()`); API in `internal/api/alerts.go`

## Authentication

Admin access is a session cookie or an API key; per-site keys are read-only for
one site. Full detail on [access](access.md).

Fails closed: the daemon refuses to start with neither an admin key nor an admin
account, rather than serving an unauthenticated API.

**Code:** `internal/auth/` (`HashPassword()`, `NewSessionToken()`, `Limiter`), `internal/api/auth.go` (`handleLogin()`, `sessionUser()`), `internal/storage/{admin,session}.go`
