# Rollout runbook — admin UI

Every deployment action needed to take the server at `monitor.pinkcrab.co.uk`
from the current key-only API to the admin UI build. **Nothing here is executed
until all phases are complete** — this file accumulates as each phase lands, and
then gets run top to bottom in one sitting.

`docs/DEPLOY.md` remains the guide for a *first* install. This file covers only
the upgrade.

> **Conventions.** Commands marked **[local]** run on your workstation, **[server]**
> on the VPS. The examples assume the SSH config alias from §0; substitute your
> own host if you use one.

---

## Status

| Phase | Work | Deployment steps written |
|-------|------|--------------------------|
| 1. Relocate API under `/api/` | ✅ done | ✅ §2, §7 |
| 2. Admin login and sessions | ✅ done | ✅ §3, §5 |
| 3. Alert engine | ✅ done | ✅ §6 |
| 4. Dashboard endpoints | ✅ done | ✅ §6e |
| 5. The SPA | ✅ done | ✅ §9 |
| 6. Deployment / Apache | ✅ done | ✅ §10, §11 |

---

## 0. Prerequisites

Commands below use the `ionos-vps` SSH alias. Confirm it works before starting:

```sh
ssh ionos-vps 'systemctl is-active uptime-monitor'
```

### The server, as surveyed

This runbook is written against the actual box, not a generic one:

| | |
|---|---|
| Host | `sleepy-payne` · 87.106.69.203 · Ubuntu 24.04 · **x86_64** |
| Panel | Plesk Obsidian 18.0.80.5 |
| Web server | **Apache serves 80 and 443 directly — nginx is inactive** |
| Subscription | `monitor.pinkcrab.co.uk`, its own subscription (Domain ID 1) |
| Document root | `/var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs` |
| System user | `uptime.pinkcrab.co.u_tf5ax5n0509`, suexec group `psacln` |
| Certificate | Cloudflare Origin, valid to 2041, bound by Plesk |
| Apache modules | `proxy`, `proxy_http`, `headers`, `ssl`, `rewrite` all loaded already |
| Monitored sites | one: `english-stamp` |
| Data | 4.2 MB in `/var/lib/uptime-monitor` |
| Also on this box | `pinkcrab.co.uk`, `crm.pinkcrab.co.uk`, `server.cide.tech` — **do not disturb** |

Because Apache is shared with three other production sites, every Apache-side
change goes through the Plesk panel, which validates before applying and reverts
a broken block rather than taking the server down.

---

## 1. Back up before touching anything

**[server]** The registry database gains tables in phases 2 and 3. Migrations
run in transactions and are one-way — there is no down-migration — so take a
copy first.

```sh
ssh ionos-vps 'systemctl stop uptime-monitor && \
  tar czf /root/uptime-monitor-backup-$(date +%F-%H%M).tar.gz \
    -C /var/lib uptime-monitor && \
  systemctl start uptime-monitor && \
  ls -lh /root/uptime-monitor-backup-*.tar.gz'
```

Stopping first guarantees a consistent copy — SQLite WAL files are being written
continuously while the service runs.

To roll back at any point: stop the service, restore the tarball over
`/var/lib/uptime-monitor`, put the previous binary back, start.

**Keep the previous binary.** Before overwriting it in §2:

```sh
ssh ionos-vps 'cp /opt/uptime-monitor/monitor /root/monitor.previous'
```

---

## 2. Build and install the new binary

**[local]** Build for the server's architecture and verify before copying — a
wrong-architecture binary fails at `systemctl start` with `status=203/EXEC`.

```sh
cd ~/Projects/uptime-monitor-api && make linux && file ./monitor
```

Expect `ELF 64-bit LSB executable, x86-64`.

```sh
scp monitor ionos-vps:/tmp/monitor
```

**[server]**

```sh
ssh ionos-vps 'mv /tmp/monitor /opt/uptime-monitor/monitor && \
  chown uptime:uptime /opt/uptime-monitor/monitor && \
  chmod 750 /opt/uptime-monitor/monitor'
```

Don't restart yet — §3 adds environment variables the new binary needs.

### What this binary already contains

- **Phase 1** — API served under `/api/`, legacy unprefixed paths still answering.
- Uptime percentage and `avg_ms` rounded to 2 decimal places.
- `avg_ms` now averages successful checks only, matching the percentiles.

> `avg_ms` values will shift after this deploy. The figure is computed at query
> time, so historical windows are recalculated under the new rule. Nothing
> stored changes.

---

## 3. Configure the admin account and session secret

**[server]** Phase 2 adds a username/password login. The first account is seeded
from the environment on first boot, then the seed variables are ignored.

Choose a strong password (this is the login to your monitoring for the whole
estate):

```sh
openssl rand -base64 24
```

Append to the env file, which stays root-only:

```sh
ssh ionos-vps 'cat >> /etc/uptime-monitor.env <<EOF
UPTIME_ADMIN_USER=stuart
UPTIME_ADMIN_PASSWORD=PASTE_THE_GENERATED_PASSWORD
EOF
chmod 600 /etc/uptime-monitor.env && chown root:root /etc/uptime-monitor.env'
```

> **Seeding happens once.** The password is hashed with bcrypt into
> `admin_users` on first start and the plaintext is never stored. Changing
> `UPTIME_ADMIN_PASSWORD` later has no effect — use §5 to change the password.
>
> Remove the two seed lines from the env file once you have logged in
> successfully, so the plaintext isn't left sitting on disk.

### Restart and confirm the migration ran

```sh
ssh ionos-vps 'systemctl restart uptime-monitor && sleep 2 && \
  systemctl status uptime-monitor --no-pager | head -12'
```

```sh
ssh ionos-vps 'journalctl -u uptime-monitor -n 30 --no-pager'
```

Look for `auth: seeded initial admin user` on this first start. If instead you
see `auth: refusing to start: no admin credential configured`, neither
`UPTIME_API_KEY` nor an admin user is set — the service now fails closed rather
than serving an open API (see §4).

---

## 4. Verify authentication

**[server]** From the box, over loopback:

```sh
ssh ionos-vps 'curl -s http://127.0.0.1:8080/api/health'
```

Unauthenticated config access must be refused:

```sh
ssh ionos-vps 'curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8080/api/sites'
```

Expect **401**. Before this release, an instance with no key configured returned
**200** here and allowed `DELETE`. If you see 200, the new binary is not running.

The existing admin key must still work, so machine consumers are unaffected:

```sh
ssh ionos-vps 'curl -s -o /dev/null -w "%{http_code}\n" \
  -H "X-API-Key: $(grep UPTIME_API_KEY /etc/uptime-monitor.env | cut -d= -f2)" \
  http://127.0.0.1:8080/api/sites'
```

Expect **200**.

**[local]** Log in through the public hostname and confirm a session cookie is
issued:

```sh
curl -s -i -X POST https://monitor.pinkcrab.co.uk/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"stuart","password":"YOUR_PASSWORD"}' | grep -i 'set-cookie\|HTTP/'
```

Expect `HTTP/1.1 200` and a `Set-Cookie: uptime_session=...` carrying `HttpOnly`,
`Secure`, and `SameSite=Lax`.

Then confirm the cookie authenticates on its own, with no API key:

```sh
curl -s -c /tmp/mon-cookies -X POST https://monitor.pinkcrab.co.uk/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"stuart","password":"YOUR_PASSWORD"}' >/dev/null && \
curl -s -b /tmp/mon-cookies https://monitor.pinkcrab.co.uk/api/auth/me
```

Expect `{"username":"stuart", ...}`. Clean up: `rm -f /tmp/mon-cookies`.

> **`Secure` cookies require HTTPS.** Logging in over plain HTTP silently gets no
> cookie stored. Always test against `https://monitor.pinkcrab.co.uk`, never the
> loopback address.

---

## 5. Password changes and lockout recovery

Change the password once logged in:

```sh
curl -s -b /tmp/mon-cookies -X POST https://monitor.pinkcrab.co.uk/api/auth/password \
  -H 'Content-Type: application/json' \
  -d '{"current_password":"OLD","new_password":"NEW"}'
```

Changing the password revokes every existing session, so you will be logged out
everywhere and must log in again.

**Locked out?** Reset from the server. The binary sets the password directly in
the database and exits without starting the service:

```sh
ssh ionos-vps '/opt/uptime-monitor/monitor -data /var/lib/uptime-monitor \
  -set-password stuart'
```

It prompts on the terminal so the password never lands in shell history or `ps`.

**Rate limiting.** Five failed attempts for a username or from an IP within 15
minutes blocks further attempts for that window, returning **429**. The counter
is in memory only, so `systemctl restart uptime-monitor` clears it.

---

## 6. Alerts — Phase 3

Alerting is inert until a channel and a rule exist, so nothing fires
unexpectedly on deploy. If SMTP is unconfigured the service still starts and
still logs deliveries — it just cannot send, which the log states at boot.

### 6a. SMTP credentials

**[server]** Add to the same root-only env file. `587` is submission with
STARTTLS; use `465` only for implicit TLS.

```sh
ssh ionos-vps 'cat >> /etc/uptime-monitor.env <<EOF
UPTIME_SMTP_HOST=smtp.your-provider.com
UPTIME_SMTP_PORT=587
UPTIME_SMTP_USER=your-smtp-username
UPTIME_SMTP_PASS=your-smtp-password
UPTIME_SMTP_FROM=uptime@pinkcrab.co.uk
EOF
chmod 600 /etc/uptime-monitor.env'
```

```sh
ssh ionos-vps 'systemctl restart uptime-monitor &&   journalctl -u uptime-monitor -n 20 --no-pager | grep alerts:'
```

Expect `alerts: smtp configured (smtp.your-provider.com:587, from …)`. If it
instead says *not configured*, the variables did not reach the process — check
`EnvironmentFile=` in the unit.

> **STARTTLS is mandatory.** On any port other than 465 the sender refuses to
> continue if the server does not offer STARTTLS, rather than sending your
> password in clear. `UPTIME_SMTP_INSECURE=true` exists for a self-signed mail
> certificate but disables verification — avoid it.

### 6b. Create a channel and prove it works

**[local]** Using the session cookie from §4, or the admin key:

```sh
curl -s -X POST https://monitor.pinkcrab.co.uk/api/alerts/channels   -H "X-API-Key: $ADMIN_KEY" -H 'Content-Type: application/json'   -d '{"name":"Stuart","target":"stuart@lambert.zone"}'
```

Note the returned `id`, then send a real test message — this is the step that
proves the credentials, not just the configuration:

```sh
curl -s -X POST https://monitor.pinkcrab.co.uk/api/alerts/channels/1/test   -H "X-API-Key: $ADMIN_KEY"
```

Expect `{"status":"sent","target":"…"}` **and an email arriving**. A `502` means
the SMTP exchange failed; the response carries the server's own error.

### 6c. Create the rules

Omitting `site_id` covers every site, including ones added later. Do this once
per kind against the same channel:

```sh
for kind in site_down site_recovered ssl_expiring; do
  curl -s -X POST https://monitor.pinkcrab.co.uk/api/alerts/rules     -H "X-API-Key: $ADMIN_KEY" -H 'Content-Type: application/json'     -d "{\"channel_id\":1,\"kind\":\"$kind\",\"confirm_after\":2}"; echo
done
```

> **Pair `site_recovered` with `site_down` on the same channel.** A channel is
> only told a site recovered if that same channel was told it went down, so a
> recovery-only rule stays permanently silent.

`confirm_after: 2` holds the down alert until two consecutive failed checks, so
a single blip sends nothing. At the default 60-second interval that delays a
genuine alert by about a minute.

### 6d. Verify end to end

Point a throwaway site at something that will fail, wait two intervals, and
watch the delivery log:

```sh
curl -s "https://monitor.pinkcrab.co.uk/api/alerts/deliveries?limit=20"   -H "X-API-Key: $ADMIN_KEY"
```

Each row carries `status` (`sent` / `failed` / `pending`), `attempts`, and the
SMTP `error` when one occurred.

```sh
ssh ionos-vps 'journalctl -u uptime-monitor --since "1 hour ago" | grep alerts:'
```

Watch for `alerts: queue full` — it means events were dropped because delivery
could not keep up. It has never appeared in testing; if it does, the SMTP server
is hanging.

### 6e. Dashboard endpoints — Phase 4

Purely additive: two new read endpoints, no schema change, no configuration.
Verify they answer once the binary from §2 is running.

```sh
curl -s "https://monitor.pinkcrab.co.uk/api/sites/overview?window=24h" \
  -H "X-API-Key: $ADMIN_KEY"
```

Expect an array with one object per site, each containing `site`, `up`,
`uptime_percent`, and `checks`.

```sh
curl -s "https://monitor.pinkcrab.co.uk/api/sites/YOUR_SITE_ID/series?window=7d&buckets=48" \
  -H "X-API-Key: $ADMIN_KEY"
```

Expect exactly 48 points. Buckets before monitoring began report `"checks": 0`
— that is a real gap, not a fault.

> `/api/sites/overview` is **admin only**, unlike the other `/sites/{id}/…` read
> endpoints. A per-site key gets 401 here by design, since the rollup would
> otherwise let one consumer enumerate every site you monitor.

---

## 7. Retire the legacy API paths

Do this **after** every consumer has moved to `/api/`, not during the upgrade.
There are now two places serving the old paths — the Plesk proxy and the daemon
— and they must be retired in this order.

### 7a. Find out what is still calling them

**[server]** The daemon names the caller on every legacy-path request,
rate-limited to one line per path per hour:

```sh
ssh ionos-vps 'journalctl -u uptime-monitor --since "7 days ago" | grep DEPRECATED | tail -20'
```

At the time of survey the access log showed `/sites/english-stamp/uptime`,
`/metrics` and one `/latency` — arriving through Cloudflare, so the log records
Cloudflare's addresses rather than the true origin. Identify the consumer from
your own side and point it at `https://monitor.pinkcrab.co.uk/api/sites/...`.

(`/sites/english-stamp/latency` is not a real endpoint and returns 404 — worth
fixing in whatever calls it while you are there. The equivalent data is in
`/api/sites/english-stamp/metrics`.)

### 7b. Remove the transitional proxies

Once the log is quiet, delete these lines from the Plesk **Additional HTTPS
directives** and apply. Note the `/sites` pair has no trailing slash, so that
bare `/sites` is covered as well as `/sites/...`:

```apache
ProxyPass        /sites http://127.0.0.1:8080/sites
ProxyPassReverse /sites http://127.0.0.1:8080/sites
ProxyPass        /health http://127.0.0.1:8080/health
ProxyPassReverse /health http://127.0.0.1:8080/health
```

**Also delete the two Cloudflare Access bypass applications** for
`monitor.pinkcrab.co.uk/sites` and `/health`. They exist only to let machine
callers reach the pre-`/api/` paths without an Access session. Left in place
they are bypasses for paths that no longer exist — harmless today, but if
either path is ever reintroduced it would be unauthenticated at the edge, which
is exactly the sort of stale rule nobody remembers writing.

The `/api` bypass **stays** — that is what lets your consumers reach the API
with just their key, and no Access service token.

Confirm the old paths no longer reach the API — they now fall through to the SPA:

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' https://monitor.pinkcrab.co.uk/health
```

`200 text/html` here is correct **at this point**, and is exactly why 7a comes
first: any consumer still on these paths would now be silently receiving the app
shell instead of JSON, with a 200 status and no error to alert anyone.

And confirm the replacement still works:

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -H "X-API-Key: $SITE_KEY" https://monitor.pinkcrab.co.uk/api/sites/english-stamp/uptime
```

`200 application/json`.

> **`/health` has moved too.** Anything pointing an external uptime check at
> `https://monitor.pinkcrab.co.uk/health` must move to `/api/health` before this
> step, or it starts reporting the site as healthy while actually receiving the
> SPA shell — a monitor that cannot fail is worse than no monitor.

### 7c. Turn the paths off in the daemon

Belt and braces, so the old routes stop existing even over loopback:

```sh
ssh ionos-vps 'sed -i "s|-addr 127.0.0.1:8080|-addr 127.0.0.1:8080 \\\n  -legacy-routes=false|" \
  /etc/systemd/system/uptime-monitor.service && \
  systemctl daemon-reload && systemctl restart uptime-monitor'
```

Verify on the box:

```sh
ssh ionos-vps 'for p in /health /api/health; do \
  printf "%-14s " $p; curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8080$p; done'
```

Expect `404` then `200`.

### 7d. Tighten the cache rule

With the legacy paths gone, narrow the `no-store` rule in the Plesk directives
so it no longer names them:

```apache
<LocationMatch "^/api/">
    Header always set Cache-Control "no-store"
</LocationMatch>
```

---

## 8. Rollback

If anything above goes wrong:

```sh
ssh ionos-vps 'systemctl stop uptime-monitor && \
  cp /root/monitor.previous /opt/uptime-monitor/monitor && \
  chown uptime:uptime /opt/uptime-monitor/monitor && \
  rm -rf /var/lib/uptime-monitor && \
  tar xzf /root/uptime-monitor-backup-*.tar.gz -C /var/lib && \
  systemctl start uptime-monitor'
```

The old binary ignores the new tables, so restoring the database is only
necessary if a migration failed — but restoring both returns you to a known
state.

Remove the phase-2 lines from `/etc/uptime-monitor.env` as well; the old binary
ignores them, but leaving a plaintext password on disk serves no purpose.

---

## 9. Build and deploy the admin UI — Phase 5

The UI is a separate artefact from the Go binary, with its own toolchain and its
own deploy step. It is **not** embedded in the binary: build it here, copy the
static files to the web server's docroot, and Apache serves them at `/` while
proxying `/api/` to the daemon (§10).

### 9a. Build

**[local]** Node 20 or newer. First time only:

```sh
cd ~/Projects/uptime-monitor-api && make web-install
```

Then, for every deploy:

```sh
cd ~/Projects/uptime-monitor-api && make web
```

Output lands in `web/dist` — roughly 84KB gzipped, no external requests at
runtime (no CDN, no web fonts), so the UI works on a locked-down network.

Confirm the build produced hashed asset names, which is what makes the caching
rule in §10 safe:

```sh
ls ~/Projects/uptime-monitor-api/web/dist/assets/
```

Expect files like `index-CTb30DDE.js`.

### 9b. Copy into the Plesk document root

There is nothing to create: the subscription already exists and its document
root currently holds Plesk's default page, which the sync replaces.

**[local]** `--delete` removes the previous build's content-hashed assets, which
would otherwise accumulate forever. `--exclude` protects Plesk's own artefacts
that live in the document root but are not ours:

```sh
rsync -av --delete --exclude '.well-known' ~/Projects/uptime-monitor-api/web/dist/ ionos-vps:/var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs/
```

**[server]** Then hand permissions back to Plesk. Apache serves this domain
under suexec as `uptime.pinkcrab.co.u_tf5ax5n0509`, and ownership differs
between files and directories — rather than guessing, let Plesk restore its own
model. Scoped to this one domain so no other subscription is touched:

```sh
ssh ionos-vps 'plesk repair fs monitor.pinkcrab.co.uk -y'
```

Skipping this is the usual cause of a 403 where the UI should be.

### 9c. Check it before changing Apache

**[server]** The files are static, so a quick local check catches a bad sync:

```sh
ssh ionos-vps 'ls -la /var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs/ && head -c 120 /var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs/index.html'
```

Expect `index.html` plus an `assets/` directory, and the file to begin
`<!doctype html>` — not Plesk's "Domain Default page".

The UI is not reachable in a browser until §10, because Apache currently proxies
every path to the daemon and never consults the document root.

> **Deploy order matters.** The UI calls `/api/sites/overview` and
> `/api/sites/{id}/series`, which only exist in the Phase 4 binary. Install the
> binary (§2) before the UI, or the dashboard loads and then errors.

### 9d. What the UI covers

- **Dashboard** — every site with status, uptime, average response, a latency
  sparkline, last check, and certificate expiry. Polls every 30 seconds.
- **Site detail** — uptime bars, a p95 latency chart, incident history, recent
  errors, and delete (with an option to purge collected history).
- **Add/edit site** — every field of the check configuration, with the
  per-check options revealed only when that check is enabled.
- **Alerts** — destinations, rules, a test send, and the delivery log with
  failure reasons.
- **Settings** — change password, and the `-set-password` recovery command.

Two behaviours are surfaced deliberately, because both are easy to get wrong:
a destination with a "site down" rule but no "site recovers" rule shows a
warning on the Alerts page, and a newly generated per-site key is shown on a
screen you must dismiss, since it can never be retrieved again.

---

## 10. Switch the Plesk directives — Phase 6

Today `vhost_ssl.conf` proxies **everything** to `127.0.0.1:8080`, which is why
the document root is never consulted. This step narrows the proxy so the SPA is
served at `/` and only the API paths are forwarded.

Apache modules are already loaded on this server — there is no `a2enmod` step —
and Plesk owns the vhost, so there is no site file to install and no
`systemctl reload apache2`. Everything happens in the panel.

> ### Read this before pasting
>
> **Keep the transitional `/sites/` and `/health` proxies.** Something is calling
> `/sites/english-stamp/uptime` and `/metrics` through Cloudflare. The daemon
> still answers those paths, but only what the proxy forwards ever reaches it.
> Narrow the proxy to `/api/` alone and those requests fall through to the
> document root, where `FallbackResource` returns the SPA's `index.html` with
> **HTTP 200** — so the consumer silently starts receiving HTML instead of JSON
> rather than failing loudly. The block in
> `deploy/plesk-additional-https-directives.conf` keeps them; §7 retires them
> later, in the right order.

### 10a. Enable mod_remoteip so the logs show real IPs

Cloudflare terminates TLS, so today every line in `access_ssl_log` shows a
Cloudflare edge address — which is why the survey could not identify the caller
of `/sites/english-stamp/uptime`. `mod_remoteip` rewrites `%h` from
`CF-Connecting-IP`, putting the true address in the log you already have.

The module is present but not loaded. **[server]**:

```sh
ssh ionos-vps 'a2enmod remoteip && apache2ctl configtest'
```

> **This is the one step that touches Apache server-wide**, and loading a module
> needs a graceful restart rather than a reload. Other sites keep serving
> through it, but do it at a quiet moment:
>
> ```sh
> ssh ionos-vps 'systemctl reload apache2 && sleep 2 && systemctl is-active apache2'
> ```
>
> Loading the module also activates Plesk's own pre-staged `<IfModule
> mod_remoteip.c>` block, which trusts only this server's addresses and reads
> `X-Forwarded-For`. That is for an nginx-in-front arrangement; nginx is
> inactive here, so it has no effect. The Cloudflare configuration is scoped to
> the monitor vhost by the directives in §10b, and does not alter logging for
> `pinkcrab.co.uk`, `crm.pinkcrab.co.uk`, or `server.cide.tech`.

### 10b. Point the daemon at the same header

The daemon writes its own logs — failed logins, legacy-path deprecation
warnings — and keys the login rate limiter by address. It must not read
`X-Forwarded-For`, which Cloudflare appends to rather than replaces, so a client
can forge its leftmost value and rotate past the per-IP throttle. It defaults to
trusting no header at all; point it at `CF-Connecting-IP` explicitly.

**[server]** Add the flag to the unit:

```sh
ssh ionos-vps 'sed -i "s|-addr 127.0.0.1:8080|-addr 127.0.0.1:8080 \\\n  -real-ip-header=CF-Connecting-IP|" \
  /etc/systemd/system/uptime-monitor.service && \
  systemctl daemon-reload && systemctl restart uptime-monitor && sleep 2 && \
  systemctl is-active uptime-monitor'
```

Confirm it took effect — the startup line reports the setting:

```sh
ssh ionos-vps 'journalctl -u uptime-monitor -n 5 --no-pager | grep "real-ip-header"'
```

Expect `real-ip-header=CF-Connecting-IP`, not `none`.

> Forging that header is only impossible for traffic that genuinely passes
> through Cloudflare. Your origin still answers directly on
> `87.106.69.203:443`, so someone bypassing Cloudflare could assert any value.
> Restricting origin 443 to Cloudflare's ranges closes that, and is worth doing
> regardless — but it is separate work, not part of this rollout.

### 10c. Paste the directives

**[local]** The block to paste is committed, with the reasoning inline:

```sh
cat deploy/plesk-additional-https-directives.conf
```

In the panel: **Websites & Domains → monitor.pinkcrab.co.uk → Apache & nginx
Settings → Additional HTTPS directives**. Replace the four lines currently there
with the whole file, then **Apply**.

Plesk validates the configuration before committing it and rejects a broken
block rather than applying it, so a mistake here cannot take down
`crm.pinkcrab.co.uk`, `pinkcrab.co.uk`, or `server.cide.tech`. If Plesk reports
an error, nothing has changed — fix and retry.

Leave *Additional HTTP directives* alone; the existing permanent redirect to
HTTPS is already correct.

> **HSTS is commented out** in the committed block, pending a decision. It is a
> one-year commitment: once a browser sees it, it refuses plain HTTP for this
> hostname until it lapses. Uncomment it only if you want that.

### 10d. Verify the split

**[local]** Six checks. The third and fourth are the ones that catch the
mistakes this configuration is prone to:

```sh
for p in /api/health / /alerts /api/nope /health /assets/; do printf '%-14s ' "$p"; curl -s -o /dev/null -w '%{http_code}  %{content_type}\n' "https://monitor.pinkcrab.co.uk$p"; done
```

| Path | Expected | Why it matters |
|------|----------|----------------|
| `/api/health` | `200 application/json` | API still reachable under its prefix |
| `/` | `200 text/html` | SPA is being served, not the API |
| `/alerts` | `200 text/html` | client-side route resolves via `FallbackResource` |
| `/api/nope` | `404 application/json` | **not** `200 text/html` — proves the fallback is scoped inside `<Directory>` and is not swallowing API paths |
| `/health` | `200 application/json` | transitional legacy path still proxied |
| `/assets/` | `403` or `404` | directory listing is off |

Then confirm the existing consumer's exact path still returns JSON:

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -H "X-API-Key: $ADMIN_KEY" https://monitor.pinkcrab.co.uk/sites/english-stamp/uptime
```

`200 application/json`. If this returns `text/html`, the transitional proxies
are missing and the consumer is now silently broken.

### 10e. Verify the headers

```sh
curl -sI https://monitor.pinkcrab.co.uk/ | grep -iE 'cache-control|content-security|x-frame|x-content-type'
```

| Path | Cache-Control |
|------|---------------|
| `/`, `/alerts`, any route | `no-cache, must-revalidate` |
| `/assets/*` | `public, max-age=31536000, immutable` |
| `/api/*`, `/sites/*`, `/health` | `no-store` |

Hashed asset names are what make the year-long cache safe: a new build changes
the filename, so a stale entry is never consulted. Everything else must
revalidate, or a browser stays pinned to the previous deploy.

### 10f. Confirm the neighbours are unaffected

The whole point of going through the panel is that these three are untouched.
Check anyway:

```sh
ssh ionos-vps 'apache2ctl configtest; for d in pinkcrab.co.uk crm.pinkcrab.co.uk server.cide.tech; do printf "%-26s " $d; curl -sk -o /dev/null -w "%{http_code}\n" -m 10 --resolve $d:443:87.106.69.203 https://$d/; done'
```

`Syntax OK`, then `401`, `200`, `303` — the baseline recorded before any change.

### 10g. Confirm the logs now name real callers

Give it a few minutes of traffic, then:

```sh
ssh ionos-vps 'awk "{print \$1}" /var/www/vhosts/system/monitor.pinkcrab.co.uk/logs/access_ssl_log | sort | uniq -c | sort -rn | head'
```

Before this rollout every line showed a Cloudflare address (`172.64.x`,
`172.71.x`). They should now be the real client addresses. The daemon's own log
agrees:

```sh
ssh ionos-vps 'journalctl -u uptime-monitor --since "1 hour ago" | grep -E "DEPRECATED|auth:" | tail'
```

That is what identifies whatever is still calling `/sites/english-stamp/uptime`,
so §7 can proceed on evidence rather than assumption.

## 11. Routine deploys from here

Everything above is one-time. Subsequent releases are one command:

```sh
./deploy/deploy.sh
```

It runs the tests, builds the binary, refuses to continue if the architecture is
wrong, backs up the previous binary, restarts the service, health-checks over
loopback, builds and rsyncs the UI, and finally checks the public endpoints.

```sh
./deploy/deploy.sh --binary-only
```

```sh
./deploy/deploy.sh --ui-only
```

The UI alone needs no service restart, so `--ui-only` is a zero-downtime
front-end deploy.

### Final checklist

Work through this once after the first full rollout:

- [ ] `https://monitor.pinkcrab.co.uk/` loads the dashboard, not Plesk's default page
- [ ] Signing in works, and the session survives a page reload
- [ ] `/api/sites` returns **401** without credentials (§4)
- [ ] The existing admin key still works
- [ ] **`/sites/english-stamp/uptime` still returns `application/json`** — if it
      returns `text/html`, the transitional proxies are missing and your existing
      consumer is silently broken (§10d)
- [ ] `/api/nope` returns `404 application/json`, not the HTML shell (§10d)
- [ ] `pinkcrab.co.uk`, `crm.pinkcrab.co.uk` and `server.cide.tech` still answer
      `401`, `200`, `303` as they did before (§10f)
- [ ] A test alert actually arrives by email (§6b)
- [ ] The seed lines are removed from `/etc/uptime-monitor.env` (§3)
- [ ] A backup tarball exists in `/root`, and `/root/monitor.previous` (§1)
- [ ] Decided on HSTS, and either uncommented it or left it out deliberately
- [ ] `access_ssl_log` shows real client addresses, not Cloudflare's (§10g)
- [ ] The startup line reports `real-ip-header=CF-Connecting-IP` (§10b)

Then, on a later day, §7 — retire the legacy paths.
