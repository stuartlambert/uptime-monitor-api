---
title: FAQ
updated: 2026-09-25
---

# FAQ

## How do I add a site?

In the UI: **Add site** on the dashboard. Fill in name and URL, enable the checks
you want, and tick *Generate a read key* if a consumer needs one — it is shown
once on save and cannot be retrieved afterwards.

By API, see [the example](api.md#example).

## How do I give someone access to one site's data?

Edit the site, tick *Generate a read key*, save, and copy the key from the screen
that follows. It reads that one site and nothing else — never hand out the admin
key. A lost key cannot be recovered (only a SHA-256 is stored); generate a
replacement and the old one stops working at once.

## I'm locked out of the admin UI

Five failed logins for a username or from an IP block sign-in for 15 minutes
(`DefaultMaxAttempts` and `DefaultWindow` in `internal/auth/ratelimit.go`). The
counter is in memory, so a restart clears it:

```bash
ssh ionos-vps 'systemctl restart uptime-monitor'
```

If the password itself is lost, reset it — it prompts, so nothing lands in shell
history:

```bash
ssh ionos-vps '/opt/uptime-monitor/monitor -data /var/lib/uptime-monitor -set-password stuart'
```

## Cloudflare Access won't let me in

Check your email is on the Allow policy in Zero Trust → Access controls →
Applications → `Uptime Monitor`. If Cloudflare itself is unreachable, tunnel
past it:

```bash
ssh -L 8080:127.0.0.1:8080 ionos-vps
```

then open <http://127.0.0.1:8080>. That bypasses Cloudflare, Access and Apache.

## I can't reach the Plesk panel

Home IP is dynamic and the panel proxy is restricted to one address. From
wherever you are now:

```bash
ssh ionos-vps /root/allow-my-ip.sh
```

On a VPN it allowlists the exit IP — disconnect first, since a shared commercial
exit is a poor thing to trust.

## Alerts aren't arriving

1. Is SMTP configured? `journalctl -u uptime-monitor | grep alerts:` should say
   `smtp configured (smtp.gmail.com:587, …)` at startup.
2. Does a channel **and** a rule exist? Neither alone sends anything.
3. Send a test from the Alerts page — the SMTP error comes back verbatim. `535`
   is a bad app password, `553`/`554` a `From` mismatch.
4. Check the delivery log for `failed` rows, and check spam.

## I got a "down" email but no "recovered" one

A channel is only told a site recovered if that same channel was told it went
down. Add a *Site recovers* rule for the same destination — the Alerts page warns
when this is missing.

## A blip didn't alert me

By design. `confirm_after` (default 2) holds the alert until that many
consecutive failed checks, so a single failure sends nothing. Lower it to 1 on
the rule if you want every blip.

## The dashboard shows "No data" for a new site

Only for a second or two. Saving a site calls `scheduler.Manager.Start()`, whose
loop runs one check immediately rather than waiting out the interval. `up` is
`null` until that first check completes — distinct from `false`, which means
down. If it stays `null`, the site is disabled or the daemon is not running.

## Why did avg_ms change when nothing happened?

Latency figures cover successful checks only and are computed at query time, so
changing the window changes the figure — the same period can read differently at
`24h` and `7d` if failures fell in between.

Two deploys also moved it historically: rounding to 2dp, and excluding failed
checks from the mean so it agrees with the percentiles. Both recalculated past
windows. Nothing stored ever changed.

## How do I see every incident, not just recent ones?

**View all** on the site detail page, or go straight to `/site/<id>/history`.
The panels on the detail page are capped; the history page is not.

## How do I pause a site without losing its history?

Untick *Actively check this site*. Deleting removes the configuration; only
*Also delete its collected history* (`?purge=true`) removes the data file.

## How big will this get?

About 30 MB per site per year at one check a minute; nothing is pruned. See
`docs/STORAGE.md` in the repo.

## Can I run it against a local or internal host?

Not as configured — `-block-private-targets` refuses private, loopback and
link-local addresses as an SSRF guard (`blockPrivateAddr()` in
`internal/checker/checker.go`). Remove the flag from the unit to allow it,
accepting that anyone who can add a site can then probe the internal network.

## Is a fresh clone of the repo enough to deploy?

Yes. Everything that runs in production is committed, including `deploy/`.
Clone, `make linux`, and `./deploy/deploy.sh`.
