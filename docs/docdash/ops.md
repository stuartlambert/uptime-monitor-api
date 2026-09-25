---
title: Operations
updated: 2026-09-25
---

# Operations

Host detail and anything server-wide is on the
[IONOS VPS](../servers/ionos-vps/index.md) pages.

| | |
|---|---|
| Binary | `/opt/uptime-monitor/monitor` — `750 uptime:uptime` |
| Data | `/var/lib/uptime-monitor/` — `registry.db` plus `<site-id>.db` per site |
| Config | `/etc/uptime-monitor.env` — `0600 root:root` |
| Unit | `/etc/systemd/system/uptime-monitor.service` |
| SPA files | `/var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs/` |
| Apache directives | Plesk → monitor.pinkcrab.co.uk → Apache & nginx Settings → Additional HTTPS directives |
| Previous binary | `/root/monitor.previous` |

## Deploy / update

From the MacBook checkout:

```bash
cd ~/Projects/uptime-monitor-api
./deploy/deploy.sh                # daemon + UI
./deploy/deploy.sh --binary-only  # daemon only
./deploy/deploy.sh --ui-only      # UI only, no restart, zero downtime
```

It runs `go vet` and `go test`, refuses to continue if the binary is not
linux/amd64, backs up the current binary, restarts the service, health-checks
over loopback, rsyncs `web/dist`, runs `plesk repair fs`, then probes the public
endpoints.

By hand, **daemon first**: the UI calls `/api/sites/overview` and `/series`, so a
new UI against an older binary loads and then errors. And `plesk repair fs
monitor.pinkcrab.co.uk -y` after any rsync is not optional — rsync lands files
owned by the local user and this domain runs under suexec, so skipping it gives a
403.

## Environment variables

Set in `/etc/uptime-monitor.env`, read via `EnvironmentFile=` so nothing appears
in `ps`.

| Name | Purpose | Default | Where it's set |
|---|---|---|---|
| `UPTIME_API_KEY` | admin API key | none | env file |
| `UPTIME_SMTP_HOST` | mail server | none | env file — `smtp.gmail.com` |
| `UPTIME_SMTP_PORT` | 587 STARTTLS, 465 implicit TLS | 587 | env file |
| `UPTIME_SMTP_USER` | SMTP username | none | env file — `hello@pinkcrab.co.uk` |
| `UPTIME_SMTP_PASS` | Google app password | none | env file |
| `UPTIME_SMTP_FROM` | envelope sender | none | env file — must match the account or a verified alias |
| `UPTIME_SMTP_INSECURE` | skip certificate verification | unset | env file — avoid |
| `UPTIME_ADMIN_USER` | first-boot seed only | none | removed after seeding |
| `UPTIME_ADMIN_PASSWORD` | first-boot seed only | none | removed after seeding |

On any port but 465 the sender **requires** STARTTLS and refuses rather than send
credentials in clear.

Flags live on the `ExecStart` line in the unit:

| Flag | Production | Purpose |
|---|---|---|
| `-data` | `/var/lib/uptime-monitor` | database directory |
| `-addr` | `127.0.0.1:8080` | listen address — never expose directly |
| `-real-ip-header` | `CF-Connecting-IP` | header carrying the true client address |
| `-block-private-targets` | set | SSRF guard |
| `-legacy-routes` | default `true` | also serve the pre-`/api/` paths |
| `-cors-origins` | default `https://portal.pinkcrab.co.uk` | exact-match browser origin allowlist |
| `-request-timeout` | default `15s` | per-check HTTP timeout |
| `-api-key` | from `UPTIME_API_KEY` | admin key |
| `-seed` | unused here | JSON file of site configs to import at startup |
| `-set-password <user>` | — | set a password and exit |

Never set `-real-ip-header` to `X-Forwarded-For` behind Cloudflare: Cloudflare
appends to any inbound value, so the leftmost entry is caller-controlled — and it
keys the login throttle.

## Backups and restore

No automatic backup. Before a risky change:

```bash
ssh ionos-vps 'systemctl stop uptime-monitor && \
  tar czf /root/uptime-monitor-backup-$(date +%F-%H%M).tar.gz -C /var/lib uptime-monitor && \
  systemctl start uptime-monitor'
```

Stopping first matters: SQLite runs in WAL mode and a clean shutdown checkpoints
it, so the archive holds complete `.db` files with no sidecars.

Restore, and roll the binary back:

```bash
ssh ionos-vps 'systemctl stop uptime-monitor && \
  cp /root/monitor.previous /opt/uptime-monitor/monitor && \
  chown uptime:uptime /opt/uptime-monitor/monitor && \
  rm -rf /var/lib/uptime-monitor && \
  tar xzf /root/uptime-monitor-backup-*.tar.gz -C /var/lib && \
  systemctl start uptime-monitor'
```

Migrations are one-way, so restore the data only if one failed; otherwise the old
binary ignores the newer tables and the binary alone is enough.

## Logs and restarts

```bash
ssh ionos-vps 'systemctl status uptime-monitor --no-pager'
ssh ionos-vps 'systemctl restart uptime-monitor'
ssh ionos-vps 'journalctl -u uptime-monitor -f'
```

| Filter | Shows |
|---|---|
| `journalctl -u uptime-monitor \| grep alerts:` | SMTP configuration and send outcomes |
| `journalctl -u uptime-monitor \| grep auth:` | logins, failures, throttling, password changes |
| `journalctl -u uptime-monitor \| grep DEPRECATED` | callers still on the legacy paths, named by IP |

The startup line reports the live configuration: prefix, legacy-routes, whether
an admin user and key exist, and the real-IP header.

## When it's down

| Symptom | Check | Fix |
|---|---|---|
| 502 from Cloudflare | `curl http://127.0.0.1:8080/api/health` on the box | daemon is down — `systemctl status uptime-monitor`, read the journal |
| Daemon won't start | `journalctl -u uptime-monitor -n 30` | `refusing to start: no admin credential` means the env file lost `UPTIME_API_KEY` |
| 403 on the UI | was the SPA just rsynced? | `plesk repair fs monitor.pinkcrab.co.uk -y` |
| UI loads then errors | binary older than the UI | deploy the daemon: `./deploy/deploy.sh --binary-only` |
| Everything 403, including through Cloudflare | Apache's origin guard | the Cloudflare IP ranges in the vhost may be stale — refresh from cloudflare.com/ips |
| Locked out of login | `journalctl -u uptime-monitor \| grep throttled` | wait 15 minutes, or restart the service to clear the counter |
| Alerts stopped | `journalctl -u uptime-monitor \| grep alerts:` | check the delivery log on `/alerts` for `failed` rows and their SMTP error |

## How requests reach it

```
browser ──▶ Cloudflare (Access on /) ──▶ Apache :443 ──┬──▶ httpdocs (SPA)
                                                       └──▶ 127.0.0.1:8080 (/api/)
```

Apache rejects connections to this vhost from outside Cloudflare's ranges, tested
with `CONN_REMOTE_ADDR` — the real TCP peer, which `mod_remoteip` does not
rewrite, so no header can forge it. Without that guard, anyone hitting the origin
IP directly would skip Cloudflare Access entirely. The guard is per-vhost, not a
host firewall, so a future site that does not sit behind Cloudflare is not
silently cut off.

## Migrations

Schema changes are ordered lists in `internal/storage/migrate.go`, tracked with
SQLite's `PRAGMA user_version`. Each runs once per database inside its own
transaction, so a failure leaves the database at its previous version. There is
no down-migration.

`registryMigrations` covers `registry.db` (sites, admin users, sessions, alert
channels/rules/deliveries); `siteMigrations` covers each `<site-id>.db` (check
results, incidents, errors, state). They apply automatically on startup.

## Tests

```bash
make test        # go test ./...
make vet
cd web && npm run typecheck
```

`internal/alerts/integration_test.go` is the one that exercises the real path —
a failing HTTP backend through the real checker, scheduler and dispatcher — and
is what proves the parts are wired to each other rather than merely correct
alone.
