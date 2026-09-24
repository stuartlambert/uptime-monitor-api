---
title: Features
updated: 2026-09-24
---

## Checks

Each site runs a fast cadence (default every 60s) and a slow one (default hourly).

| Check | Cadence | Fails when |
|---|---|---|
| HTTP status | fast | status not in `expect_status`; if empty, anything outside 2xx/3xx |
| Latency | fast | response slower than `max_ms`; `0` records timings without ever failing |
| Content | fast | body lacks a `must_contain` string, or holds a `must_not_contain` one |
| SSL | slow | certificate expired; warns within `warn_days` of expiry |
| DNS | slow | hostname does not resolve |

Any failing check marks the tick down and opens an incident. A slow response is
therefore an outage, not a separate alert class.

| Setting | Default | Bounds |
|---|---|---|
| `interval_seconds` | 60 | 5 – 86400 |
| `slow_interval_seconds` | 3600 | must be ≥ `interval_seconds` |
| `ssl.warn_days` | 14 | — |
| `latency.max_ms` | 0 (off) | — |

## What is recorded

Per site, in its own SQLite file: every check result, incidents (down periods
with cause and duration), and an error log. Uptime percentage, mean and
p50/p95/p99 latency are computed at query time.

Latency figures cover **successful checks only**, so a fast-failing 500 does not
flatter the average. Uptime and `avg_ms` are rounded to two decimal places.

Nothing is pruned. Roughly 30 MB per site per year at one check a minute; see
`docs/STORAGE.md`. Whether to add a retention cap is still open, currently
leaning no.

## Admin UI

| Page | Path | |
|---|---|---|
| Dashboard | `/` | every site: status, uptime, average response, latency sparkline, last check, certificate expiry. Polls every 30s |
| Site detail | `/site/:id` | uptime bars, p95 latency chart, incidents, recent errors, danger zone |
| History | `/site/:id/history` | full paginated incidents + errors for one site |
| Add / edit | `/site/new`, `/site/:id/edit` | every check option, revealed as each check is enabled |
| Alerts | `/alerts` | destinations, rules, delivery log, send-test |
| Settings | `/settings` | change password |

Clicking a bar on the uptime graph filters the incidents list to that bar's time
window. Both the history page and the filtering are done client-side over an
already-fetched set, so neither needs a backend change.

The UI lives under `/site/…`, deliberately not `/sites/…`, because `/sites/` is
the API's namespace and the two would collide.

## Alerting

Email over SMTP. Nothing is sent until a **channel** (a destination) and a
**rule** (what triggers a send) exist.

| Kind | Fires when |
|---|---|
| `site_down` | a site has failed `confirm_after` consecutive checks |
| `site_recovered` | a site that was announced down comes back |
| `ssl_expiring` | the certificate is within that site's `warn_days` |

A rule with no `site_id` covers every site, including ones added later.

There is no separate latency alert: a slow response is already a failed check,
so it fires `site_down`.

### Behaviour worth knowing

- **Confirmation.** `confirm_after` (default 2) holds a down alert until that
  many consecutive failures, so a single blip sends nothing.
- **One mail per incident.** Deduplication is a `UNIQUE` key claimed in the
  database before sending, so a restart mid-outage cannot re-announce it.
- **Recovery is paired per channel.** A channel is only told a site recovered if
  that same channel was told it went down — so give a channel both rules, or its
  recovery rule never fires. The Alerts page warns when this is misconfigured.
- **Certificate alerts key on the expiry date**, so an hourly check mails once,
  and a renewed certificate alerts again on its own terms.
- **A permanently failing channel stops** after its retries; the delivery row is
  kept as `failed`, which prevents a mail loop.
- **Delivery never blocks monitoring.** Events go to a bounded queue handled by
  separate workers; if it saturates, events are dropped with a log line rather
  than stalling checks.

### Current configuration

| | |
|---|---|
| Channel 1 "Pink Crab" | `hello@pinkcrab.co.uk` |
| Rules | `site_down` (confirm after 2), `site_recovered`, `ssl_expiring` — all sites |
| Monitored | `english-stamp`, `glynn-quelch` |
