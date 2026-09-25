---
title: Access
updated: 2026-09-25
---

# Access

Two layers guard the UI: Cloudflare Access at the edge, then the app's own login.
Machine callers skip Access and use an API key. Server logins are on the
[IONOS VPS access page](../servers/ionos-vps/access.md).

## Accounts

| Who | Username | Password | Role |
|---|---|---|---|
| Stuart | `stuart` | 1Password | admin — the only account |

Seeded once from `UPTIME_ADMIN_USER` / `UPTIME_ADMIN_PASSWORD` on the first boot
with no account, then bcrypt-hashed into `registry.db` and those variables
ignored for ever. Both have been removed from the env file.

Minimum 12 characters, bcrypt cost 12. The session cookie lasts 7 days with a
sliding expiry; only a SHA-256 of it is stored, so a database leak yields no
usable sessions. Changing the password revokes every session.

Login is throttled to **5 failures per username and per IP in 15 minutes**, then
429. The counter is in memory, so a restart clears a lockout:

```bash
ssh ionos-vps 'systemctl restart uptime-monitor'
```

Reset the password from the server — it prompts, so nothing reaches shell history:

```bash
ssh ionos-vps '/opt/uptime-monitor/monitor -data /var/lib/uptime-monitor -set-password stuart'
```

### Cloudflare Access

Zero Trust → Access controls → Applications. More specific paths win, so the
bypasses override the root Allow.

| Application path | Policy | Covers |
|---|---|---|
| `monitor.pinkcrab.co.uk` | Allow — `stuart@lambert.zone` | the UI |
| `monitor.pinkcrab.co.uk/api` | Bypass — Everyone | machine callers |
| `monitor.pinkcrab.co.uk/sites` | Bypass — Everyone | legacy paths, retiring |
| `monitor.pinkcrab.co.uk/health` | Bypass — Everyone | legacy path, retiring |

Sign-in is Google SSO (Google Workspace) or a one-time PIN. `/api` is bypassed
so consumers need only their API key, not an Access service token.

If Cloudflare is ever unreachable, tunnel past the whole stack:

```bash
ssh -L 8080:127.0.0.1:8080 ionos-vps    # then http://127.0.0.1:8080
```

## API keys

Two kinds, both sent as an `X-API-Key` header.

| Key | Scope | How to create |
|---|---|---|
| Admin | all config CRUD, reads any site | `UPTIME_API_KEY` in `/etc/uptime-monitor.env` |
| Per-site | read-only, that site's data endpoints | tick *Generate a read key* on the site, or `"generate_api_key": true` |

A generated key is returned **once** in that response and stored only as a
SHA-256 — it cannot be retrieved later, only rotated by generating another, which
invalidates the old one immediately. Site config responses expose only a
`has_api_key` boolean.

Hand a consumer its own site's key, never the admin key, so a leak is contained
to one site's read data.

## Where secrets live

| Secret | Where | Used by |
|---|---|---|
| Admin API key | `/etc/uptime-monitor.env` (`0600 root:root`) | machine consumers, `-api-key` |
| SMTP credentials | same file | the alert sender |
| Admin password hash, session hashes, per-site key hashes | `/var/lib/uptime-monitor/registry.db` | the daemon |
| Admin UI password | 1Password | Stuart |

Nothing sensitive is returned by the API: hashes are `json:"-"` and never
serialised.
