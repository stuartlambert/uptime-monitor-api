---
title: API
updated: 2026-09-24
---

Base URL `https://monitor.pinkcrab.co.uk/api`. JSON in, JSON out. Authenticate
with an `X-API-Key` header or the session cookie from `/auth/login`. See
[Access](access.md#api-keys) for which key to use.

Everything fails closed: only `/health` and `/auth/login` answer without a
credential, and a site with no per-site key is admin-only rather than public.

## Auth

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/auth/login` | none | exchange username + password for a session cookie |
| POST | `/auth/logout` | none | revoke the current session server-side |
| GET | `/auth/me` | admin | who the caller is |
| POST | `/auth/password` | session only | change password; revokes every session |

`/auth/password` refuses an API key — a leaked machine key cannot take over the
account.

## Sites

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/sites` | admin | list site configs |
| POST | `/sites` | admin | create a site |
| GET | `/sites/{id}` | admin | one site's config |
| PUT | `/sites/{id}` | admin | update (send the whole object; `id` immutable) |
| DELETE | `/sites/{id}` | admin | remove; `?purge=true` also deletes its history |
| GET | `/sites/overview` | admin | every site with live state and uptime |

`id` is slugified from `name` on create — "English Stamp" becomes `english-stamp`.

## Site data

Readable with that site's own key or an admin credential.

| Method | Path | Parameters |
|---|---|---|
| GET | `/sites/{id}/status` | — |
| GET | `/sites/{id}/uptime` | `window` |
| GET | `/sites/{id}/metrics` | `window` |
| GET | `/sites/{id}/series` | `window`, `buckets` |
| GET | `/sites/{id}/incidents` | `limit` |
| GET | `/sites/{id}/errors` | `since`, `limit` |
| GET | `/sites/{id}/results` | `since`, `limit` |

| Parameter | Values |
|---|---|
| `window` | `1h`, `24h`, `7d`, `30d`, `all` (default `all`; anything else falls back to `all`) |
| `buckets` | default 96, capped at 1000 |
| `limit` | default 100, capped at 10000 |
| `since` | unix seconds |

## Alerts

| Method | Path | Purpose |
|---|---|---|
| GET | `/alerts/channels` | list destinations |
| POST | `/alerts/channels` | create one |
| PUT | `/alerts/channels/{id}` | update |
| DELETE | `/alerts/channels/{id}` | delete (cascades to its rules) |
| POST | `/alerts/channels/{id}/test` | send a real test message now |
| GET | `/alerts/rules` | list rules |
| POST | `/alerts/rules` | create one |
| PUT | `/alerts/rules/{id}` | update |
| DELETE | `/alerts/rules/{id}` | delete |
| GET | `/alerts/deliveries` | send log; `?site_id=`, `?limit=` |

All admin-only. See [Features](features.md#alerting) for the rule semantics.

## Health

| Method | Path | Auth |
|---|---|---|
| GET | `/health` | none — `{"status":"ok","time":…}` |

## Creating a site

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

The response carries `api_key` **once**. Save it.

## Reading data

```bash
curl -H "X-API-Key: $SITE_KEY" \
  'https://monitor.pinkcrab.co.uk/api/sites/english-stamp/uptime?window=24h'
```

## Errors

| Status | Meaning |
|---|---|
| 400 | bad JSON, or a field failed validation (`url` is required and must be http/https) |
| 401 | missing or wrong credential |
| 404 | unknown site id, or an unmatched route (plain text) |
| 409 | a site with that id already exists |
| 429 | login throttled |
| 502 | test-send reached SMTP and it refused — the server's error is in the body |

## Legacy paths (retiring)

The API also answers without the `/api` prefix — `/sites/…`, `/health` — kept
while consumers migrate. They log a rate-limited deprecation warning naming the
caller. Retirement is `docs/ROLLOUT.md` §7, in order: confirm the log is quiet,
remove the Apache proxy lines and the two Cloudflare Access bypasses, then set
`-legacy-routes=false`.

Anything still pointing an uptime check at `/health` must move to `/api/health`
first — afterwards `/health` returns the SPA shell with a 200, so a checker would
report healthy for ever.
