---
title: Access
updated: 2026-09-24
---

Three separate credentials guard this: Cloudflare Access at the edge, the admin
login in the app, and API keys for machine callers.

## Admin UI

<https://monitor.pinkcrab.co.uk> asks for two things in sequence.

1. **Cloudflare Access** — Google SSO (Google Workspace) or a one-time PIN to an
   allowed address. Managed in Cloudflare Zero Trust → Access controls → Applications.
2. **The app's own login** — username `stuart`, password in 1Password.

Both are deliberate: Access can be bypassed only by Cloudflare, the app login
identifies who acted.

### Cloudflare Access applications

| Application path | Policy | Covers |
|---|---|---|
| `monitor.pinkcrab.co.uk` | Allow — `stuart@lambert.zone` | the UI |
| `monitor.pinkcrab.co.uk/api` | Bypass — Everyone | machine callers |
| `monitor.pinkcrab.co.uk/sites` | Bypass — Everyone | legacy paths (retiring) |
| `monitor.pinkcrab.co.uk/health` | Bypass — Everyone | legacy path (retiring) |

More specific paths win, so the three bypasses override the root Allow. `/api/`
is bypassed so consumers need only their API key, not an Access service token.

## Admin password

Seeded once from `UPTIME_ADMIN_USER` / `UPTIME_ADMIN_PASSWORD` on the first boot
where no account exists, then bcrypt-hashed into the database and those variables
ignored for ever. They have been removed from the env file.

Rules: minimum 12 characters, bcrypt cost 12, session cookie valid 7 days with a
sliding expiry. Changing the password revokes every session.

Locked out? Reset it over SSH — it prompts, so nothing reaches shell history:

```bash
ssh ionos-vps '/opt/uptime-monitor/monitor -data /var/lib/uptime-monitor -set-password stuart'
```

Login is throttled to **5 failures per username and per IP in 15 minutes**, then
429. The counter is in memory, so `systemctl restart uptime-monitor` clears a
lockout.

## API keys

| Key | Scope | Where |
|---|---|---|
| Admin key | all config CRUD, reads any site | `UPTIME_API_KEY` in `/etc/uptime-monitor.env` |
| Per-site key | read-only, one site's data endpoints | issued per site, shown once |

Send either as `X-API-Key`. A per-site key is generated with
`"generate_api_key": true` on create or update, returned **once** in that
response, and stored only as a SHA-256 — it cannot be retrieved later, only
rotated. Hand consumers a per-site key, never the admin key.

## Where the secrets live

| Secret | Location |
|---|---|
| Admin API key, SMTP credentials | `/etc/uptime-monitor.env` — `0600 root:root` |
| Admin password hash, session hashes, per-site key hashes | `/var/lib/uptime-monitor/registry.db` |
| Admin UI password | 1Password |
| SSH key for `ionos-vps` | MacBook, `~/.ssh/config` |

Nothing sensitive is ever returned by the API: key hashes are `json:"-"`, and a
site config exposes only a `has_api_key` boolean.

## Server access

```bash
ssh ionos-vps
```

Key-only — `PasswordAuthentication no`, `PermitRootLogin prohibit-password`, set
in `/etc/ssh/sshd_config.d/01-hardening.conf`.

## Plesk panel

<https://server.cide.tech> proxies to the panel on `127.0.0.1:8443`, and is
restricted at Apache to a single admin IP. Home IP is dynamic, so when it
changes:

```bash
ssh ionos-vps /root/allow-my-ip.sh          # sets the allowlist to where you are
ssh ionos-vps /root/allow-my-ip.sh --show   # print the current one
```

SSH works from any address, so it is always the way back in. The panel is also
reachable directly on `https://87.106.69.203:8443`.
