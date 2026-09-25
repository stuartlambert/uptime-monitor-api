---
title: API
updated: 2026-09-25
---

# API

Base URL `https://monitor.pinkcrab.co.uk/api`; authenticate with an `X-API-Key`
header or the session cookie from `/auth/login` — see [access](access.md#api-keys).

Everything fails closed. Only `/health` and `/auth/login` answer without a
credential, and a site with no per-site key is admin-only, not public.

## Endpoints

### Auth

| Method | Path | Auth | Body | Returns |
|---|---|---|---|---|
| POST | `/auth/login` | none | `{username, password}` | sets session cookie; `{username, expires_at}` |
| POST | `/auth/logout` | none | — | 204; revokes the session server-side |
| GET | `/auth/me` | admin | — | `{username, auth}` — `auth` is `session` or `api_key` |
| POST | `/auth/password` | session only | `{current_password, new_password}` | `{status}`; revokes every session |

`/auth/password` refuses an API key, so a leaked machine key cannot take over the
account.

### Sites

| Method | Path | Auth | Body | Returns |
|---|---|---|---|---|
| GET | `/sites` | admin | — | array of site configs |
| POST | `/sites` | admin | site config | the created site; `api_key` once if generated |
| GET | `/sites/{id}` | admin | — | one site config |
| PUT | `/sites/{id}` | admin | full site config | the updated site (`id` immutable) |
| DELETE | `/sites/{id}` | admin | — | 204; `?purge=true` also deletes its history |
| GET | `/sites/overview` | admin | — | every site with live state and uptime |

`id` is slugified from `name` on create — "English Stamp" becomes `english-stamp`.

### Site data

That site's own key, or any admin credential.

| Method | Path | Parameters | Returns |
|---|---|---|---|
| GET | `/sites/{id}/status` | — | current up/down, last check, cert expiry |
| GET | `/sites/{id}/uptime` | `window` | checks, successful, failed, `uptime_percent` |
| GET | `/sites/{id}/metrics` | `window` | counts plus `avg_ms`, p50/p95/p99 |
| GET | `/sites/{id}/series` | `window`, `buckets` | bucketed uptime and latency for charts |
| GET | `/sites/{id}/incidents` | `limit` | down periods with cause and duration |
| GET | `/sites/{id}/errors` | `since`, `limit` | stored errors, newest first |
| GET | `/sites/{id}/results` | `since`, `limit` | raw check rows |

| Parameter | Values |
|---|---|
| `window` | `1h`, `24h`, `7d`, `30d`, `all` — default `all`; anything unrecognised falls back to `all` |
| `buckets` | default 96, capped at 1000 |
| `limit` | default 100, capped at 10000 |
| `since` | unix seconds |

### Alerts

All admin-only. Semantics are on [features](features.md#alerting).

| Method | Path | Body | Returns |
|---|---|---|---|
| GET | `/alerts/channels` | — | destinations |
| POST | `/alerts/channels` | `{name, type, target, enabled}` | the created channel |
| PUT | `/alerts/channels/{id}` | same | the updated channel |
| DELETE | `/alerts/channels/{id}` | — | 204; cascades to its rules |
| POST | `/alerts/channels/{id}/test` | — | sends a real message now |
| GET | `/alerts/rules` | — | rules |
| POST | `/alerts/rules` | `{channel_id, kind, site_id, confirm_after}` | the created rule |
| PUT | `/alerts/rules/{id}` | same | the updated rule |
| DELETE | `/alerts/rules/{id}` | — | 204 |
| GET | `/alerts/deliveries` | `?site_id=`, `?limit=` | the send log |

### Health

| Method | Path | Auth | Returns |
|---|---|---|---|
| GET | `/health` | none | `{"status":"ok","time":…}` |

## Errors

JSON, one field: `{"error": "admin credentials required"}`.

| Status | Meaning |
|---|---|
| 400 | bad JSON, or a field failed validation — `url` is required and must be http/https |
| 401 | missing or wrong credential |
| 404 | unknown site id; an unmatched route returns Go's plain-text 404 instead |
| 409 | a site with that id already exists |
| 429 | login throttled; `Retry-After` gives the seconds |
| 502 | a test send reached SMTP and it refused — the server's own error is in the body |
| 503 | test send attempted with no mail transport configured |

## Example

```bash
curl -X POST https://monitor.pinkcrab.co.uk/api/sites \
  -H "X-API-Key: $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{
        "name": "My Site",
        "url": "https://example.com",
        "interval_seconds": 60,
        "generate_api_key": true,
        "checks": {
          "http":    {"enabled": true, "expect_status": [200]},
          "latency": {"enabled": true, "max_ms": 2000},
          "content": {"enabled": true, "must_not_contain": ["Exception"]},
          "ssl":     {"enabled": true, "warn_days": 14},
          "dns":     {"enabled": true}
        }
      }'
```

The response carries `api_key` once. Then read it back:

```bash
curl -H "X-API-Key: $SITE_KEY" \
  'https://monitor.pinkcrab.co.uk/api/sites/my-site/uptime?window=24h'
```

## Legacy paths

The API also answers without the `/api` prefix — `/sites/…`, `/health` — kept
while consumers migrate, controlled by `-legacy-routes` (default on). Those
requests log a rate-limited deprecation warning naming the caller:

```bash
ssh ionos-vps 'journalctl -u uptime-monitor | grep DEPRECATED'
```

Retirement is `docs/ROLLOUT.md` §7, in order: confirm the log is quiet, remove
the Apache proxy lines and the two Cloudflare Access bypasses, then set
`-legacy-routes=false`.

Move anything pointing an uptime check at `/health` to `/api/health` **first** —
afterwards `/health` falls through to the SPA and returns HTML with a 200, so a
checker would report healthy for ever.
