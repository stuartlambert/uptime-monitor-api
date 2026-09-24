---
title: Operations
updated: 2026-09-24
---

## Where everything is

| | |
|---|---|
| Host | [IONOS VPS](../servers/ionos-vps/index.md) `87.106.69.203` — `ssh ionos-vps` |
| Binary | `/opt/uptime-monitor/monitor` — `750 uptime:uptime` |
| Data | `/var/lib/uptime-monitor/` — `registry.db` plus `<site-id>.db` per site |
| Config | `/etc/uptime-monitor.env` — `0600 root:root` |
| Unit | `/etc/systemd/system/uptime-monitor.service` |
| SPA files | `/var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs/` |
| Apache directives | Plesk → monitor.pinkcrab.co.uk → Apache & nginx Settings → Additional HTTPS directives |
| Previous binary | `/root/monitor.previous` |

## Deploying

From the MacBook checkout:

```bash
cd ~/Projects/uptime-monitor-api
./deploy/deploy.sh                # daemon + UI
./deploy/deploy.sh --binary-only  # daemon only
./deploy/deploy.sh --ui-only      # UI only — no restart, zero downtime
```

It runs `go vet` and `go test`, checks the binary is linux/amd64, backs up the
old one, restarts,
health-checks over loopback, rsyncs `web/dist`, runs `plesk repair fs`, then
probes the public endpoints.

By hand, **daemon first**: the UI calls `/api/sites/overview` and `/series`, so
a new UI on an old binary loads and then errors. And `plesk repair fs
monitor.pinkcrab.co.uk -y` after any rsync is not optional — rsync lands files
owned by the local user and this domain runs under suexec, so skipping it gives
a 403.

## Configuration

Flags live on the `ExecStart` line in the unit; secrets come from the env file.

| Flag | Production value | |
|---|---|---|
| `-data` | `/var/lib/uptime-monitor` | database directory |
| `-addr` | `127.0.0.1:8080` | loopback only — never expose directly |
| `-real-ip-header` | `CF-Connecting-IP` | header carrying the true client address |
| `-block-private-targets` | set | refuses to check private/loopback addresses (SSRF guard) |
| `-legacy-routes` | default `true` | also serve the pre-`/api/` paths |
| `-cors-origins` | default `https://portal.pinkcrab.co.uk` | exact-match browser origin allowlist |
| `-request-timeout` | default 15s | per-check HTTP timeout |
| `-api-key` | from `UPTIME_API_KEY` | admin key |
| `-seed` | unused here | JSON file of site configs to import at startup |
| `-set-password <user>` | — | set a password and exit |

Never set `-real-ip-header` to `X-Forwarded-For` behind Cloudflare: Cloudflare
appends to any inbound value, so the leftmost entry is caller-controlled — and
it keys the login throttle.

| Environment variable | |
|---|---|
| `UPTIME_API_KEY` | admin API key |
| `UPTIME_SMTP_HOST` | `smtp.gmail.com` |
| `UPTIME_SMTP_PORT` | `587` (STARTTLS; `465` = implicit TLS) |
| `UPTIME_SMTP_USER` | `hello@pinkcrab.co.uk` |
| `UPTIME_SMTP_PASS` | Google app password |
| `UPTIME_SMTP_FROM` | `hello@pinkcrab.co.uk` |
| `UPTIME_SMTP_INSECURE` | unset — `true` disables certificate verification |
| `UPTIME_ADMIN_USER` / `UPTIME_ADMIN_PASSWORD` | seed only, removed after first boot |

On any port but 465 the sender **requires** STARTTLS. The daemon **refuses to
start** with neither an admin key nor an admin account, rather than silently
serving an unauthenticated API.

## Service control

```bash
ssh ionos-vps 'systemctl status uptime-monitor --no-pager'
ssh ionos-vps 'systemctl restart uptime-monitor'
ssh ionos-vps 'journalctl -u uptime-monitor -f'
ssh ionos-vps 'curl -s http://127.0.0.1:8080/api/health'
```

| Log filter | Shows |
|---|---|
| `journalctl -u uptime-monitor \| grep alerts:` | SMTP configuration and send outcomes |
| `journalctl -u uptime-monitor \| grep auth:` | logins, failures, throttling, password changes |
| `journalctl -u uptime-monitor \| grep DEPRECATED` | callers still on the legacy paths, named by IP |

The startup line reports the live configuration: prefix, legacy-routes, whether
an admin user and key exist, and the real-IP header.

## How requests reach it

```
browser ──▶ Cloudflare (Access on /) ──▶ Apache :443 ──┬──▶ httpdocs (SPA)
                                                       └──▶ 127.0.0.1:8080 (/api/)
```

Apache rejects connections to this vhost from outside Cloudflare's ranges,
tested with `CONN_REMOTE_ADDR` — the real TCP peer, which `mod_remoteip` does not
rewrite, so no header can forge it. Without it, the origin IP skips Access.

## Backups

None automatic. Before a risky change:

```bash
ssh ionos-vps 'systemctl stop uptime-monitor && \
  tar czf /root/uptime-monitor-backup-$(date +%F-%H%M).tar.gz -C /var/lib uptime-monitor && \
  systemctl start uptime-monitor'
```

Stopping first matters: WAL checkpoints on clean shutdown, so the archive holds
complete `.db` files.

## Rollback

```bash
ssh ionos-vps 'systemctl stop uptime-monitor && \
  cp /root/monitor.previous /opt/uptime-monitor/monitor && \
  chown uptime:uptime /opt/uptime-monitor/monitor && \
  systemctl start uptime-monitor'
```

Migrations are one-way; if one failed, restore the tarball over
`/var/lib/uptime-monitor` too.

## Outstanding

| | |
|---|---|
| Plesk panel on 8443 | open to the internet; needs a tunnel or an allowlist |
| 12 OS security patches | needs a window and a reboot |
| `mod_remoteip` server-wide | unblocks the Apache fail2ban jails, which today would ban Cloudflare edges |
| Retire the legacy API paths | `docs/ROLLOUT.md` §7 |
| Retention cap | open question, leaning no |
