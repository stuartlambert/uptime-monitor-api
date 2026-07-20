# Deployment guide

Step-by-step runbook for taking uptime-monitor live on a server that is
**already running other websites** — e.g. a Plesk/cPanel box, or a plain nginx
host — typically behind Cloudflare. The monitor binds to loopback and is exposed
through the web server you already run, as one more virtual host on port 443. Your
data-consuming systems talk to it over HTTPS using per-site API keys.

**Assumes:**

- A Linux server already serving other sites, with an existing web server
  (Plesk with nginx+Apache in these examples, but plain nginx works the same way)
  that **owns ports 80 and 443**.
- Cloudflare in front of your domain (recommended), or direct DNS if you prefer.
- A hostname for the monitor — a **subdomain** such as `monitor.example.com` —
  that you can add to that web server and point DNS at.

> ## Read this first — deploying onto a live web server
>
> This box exists because the alternatives waste hours and can take your other
> sites offline. On a server that already serves 443:
>
> - **Do NOT install a second reverse proxy (Caddy, a standalone nginx, …).**
>   Only one process can bind port 443. A newcomer either fails to start or, worse,
>   grabs 443 and knocks your existing sites offline. Use the web server you
>   **already** run as the reverse proxy.
> - **Do NOT invent a custom HTTPS port like `8443`.** You don't need one — a
>   subdomain shares 443 with every other site via SNI (name-based virtual
>   hosting). And on **Plesk specifically, `8443` (and `8880`/`8447`) are the
>   control-panel's own ports** — binding `8443` yourself breaks Plesk.
> - **Do NOT use a self-signed origin certificate with Cloudflare "Full (strict)".**
>   Use a free **Cloudflare Origin Certificate** (§5a), which strict mode trusts.
> - **Keep the monitor on `127.0.0.1:8080`.** Nothing public should bind 8080.

---

## About the admin key

> **The admin key is deliberately not written in this file.** This document is
> committed to git; secrets must never be. Your generated key lives in
> `uptime-monitor.env` in the repo root, which is gitignored.

The system has two tiers of key:

| Key | Scope | Who holds it |
|-----|-------|--------------|
| **Admin** (`UPTIME_API_KEY`) | Full config CRUD + read any site | You / your ops tooling |
| **Per-site** | Read-only, one site's data endpoints | Each consuming system |

Give consumers a per-site key, never the admin key, so a leak is contained to a
single site's read data.

To generate a fresh admin key at any time:

```sh
openssl rand -hex 32
```

Put it in an env file (see step 3). To rotate: replace the value and
`sudo systemctl restart uptime-monitor`.

---

## 1. Build the Linux binary

The binary is fully static (pure-Go SQLite, `CGO_ENABLED=0`), so it
cross-compiles from any machine and has no runtime dependencies on the server.

**Recommended: build on your workstation and copy the binary over** — then the
server needs no Go toolchain at all (and you avoid distro Go being too old).

Run **exactly one** of these, matching your server's CPU. ⚠️ They both write to
the same `./monitor` file, so running both leaves you with whichever ran *last* —
copy the wrong architecture up and the service dies with `status=203/EXEC`.

```sh
# Intel/AMD servers (IONOS, DigitalOcean, EC2 x86, most VPSes):
make linux          # -> ./monitor  (linux/amd64)

# ARM servers ONLY (Graviton, Ampere, Raspberry Pi):
make linux-arm64    # -> ./monitor  (linux/arm64)
```

Not sure which? Check the server: `ssh youruser@SERVER_IP uname -m` →
`x86_64` means use `make linux`; `aarch64` means use `make linux-arm64`.

Then **verify the binary before copying it** — this one check would have caught
the arch mismatch above:

```sh
file ./monitor
# amd64  -> "ELF 64-bit LSB executable, x86-64"
# arm64  -> "ELF 64-bit LSB executable, ARM aarch64"
# "Mach-O ..." means you ran `make build` (a macOS binary) — use `make linux`.
```

Windows (PowerShell), amd64:

```powershell
$env:CGO_ENABLED=0; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -ldflags="-s -w" -o monitor ./cmd/monitor
```

> **If you build *on* the server instead:** install Go **≥ 1.22** from
> <https://go.dev/dl/> (the tarball), **not** `apt install golang-go` — the
> distro package is usually older than the `go 1.22` requirement in `go.mod` and
> `make linux` will fail with `go.mod requires go >= 1.22`. Check `uname -m`
> (`x86_64` → amd64, `aarch64` → arm64) to pick the right tarball.

## 2. Copy the binary and env file to the server

```sh
scp monitor youruser@SERVER_IP:/tmp/monitor
scp uptime-monitor.env youruser@SERVER_IP:/tmp/uptime-monitor.env
```

## 3. Provision the server

Run as a dedicated unprivileged user. The **data directory
(`/var/lib/uptime-monitor`) is created automatically** by the systemd unit's
`StateDirectory=` in step 4 — you don't create or chown it here.

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin uptime
sudo mkdir -p /opt/uptime-monitor

sudo mv /tmp/monitor /opt/uptime-monitor/monitor
sudo chmod 750 /opt/uptime-monitor/monitor

# Env file holds the admin key: root-only, 0600.
sudo mv /tmp/uptime-monitor.env /etc/uptime-monitor.env
sudo chown root:root /etc/uptime-monitor.env
sudo chmod 600 /etc/uptime-monitor.env

sudo chown -R uptime:uptime /opt/uptime-monitor
```

If you'd rather create the env file by hand:

```sh
sudo tee /etc/uptime-monitor.env >/dev/null <<'EOF'
UPTIME_API_KEY=paste-your-admin-key-here
EOF
sudo chmod 600 /etc/uptime-monitor.env
```

## 4. Create the systemd service

```sh
sudo tee /etc/systemd/system/uptime-monitor.service >/dev/null <<'EOF'
[Unit]
Description=uptime-monitor
After=network-online.target
Wants=network-online.target

[Service]
User=uptime
Group=uptime
EnvironmentFile=/etc/uptime-monitor.env
# StateDirectory creates/owns /var/lib/uptime-monitor for the service user on
# every start, and makes it writable under ProtectSystem=strict automatically.
StateDirectory=uptime-monitor
ExecStart=/opt/uptime-monitor/monitor \
  -data /var/lib/uptime-monitor \
  -addr 127.0.0.1:8080
Restart=always
RestartSec=3

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now uptime-monitor
sudo systemctl status uptime-monitor --no-pager
journalctl -u uptime-monitor -f      # live logs
```

Notes:

- **Bind to `127.0.0.1:8080`** so the monitor is reachable only via the local
  reverse proxy, never directly from the network.
- The admin key is read from `UPTIME_API_KEY` via `EnvironmentFile`, so it never
  appears on the command line (where `ps` would expose it).
- Add **`-block-private-targets`** to `ExecStart` *only if* you do not intend to
  monitor internal/private hosts — it refuses connections to private, loopback,
  and link-local addresses (an SSRF guard covering redirects and DNS rebinding).

Confirm it's up locally before wiring the proxy:

```sh
curl -s http://127.0.0.1:8080/health      # -> {"status":"ok","time":...}
```

## 5. Expose the API over HTTPS (on your existing web server)

The API speaks plain HTTP and the key travels in a header, so TLS is mandatory
before it leaves the host. You already have a web server on 443 — use it. The
monitor becomes a subdomain (`monitor.example.com`) that shares 443 with your
other sites via SNI. **No new proxy, no new port.** (Re-read the box at the top
of this file if you're tempted otherwise.)

The request path once wired up:

```
browser ──TLS #1──▶ Cloudflare ──TLS #2──▶ your web server (443, SNI) ──▶ 127.0.0.1:8080
        (Cloudflare edge cert)   (origin cert)      nginx/Apache            (monitor)
```

Cloudflare's SSL only covers **hop #1** (browser ↔ Cloudflare). **Hop #2**
(Cloudflare ↔ your server) is a separate connection and must be encrypted too,
since your API key rides in the `X-API-Key` header — so the origin needs its own
certificate. That's what §5a sets up.

### 5a. Cloudflare — DNS + origin certificate

1. **DNS → Add record:** type `A`, name `monitor`, value = your server's public
   IPv4, **Proxy status: Proxied (orange cloud)**. Universal SSL then serves the
   public (edge) certificate for `monitor.example.com` automatically.
2. **SSL/TLS → Origin Server → Create Certificate** → hostname
   `monitor.example.com` (or `*.example.com`), 15-year validity. Copy the
   **certificate** and **private key** — you'll paste them into the web server in
   §5b.
3. **SSL/TLS → Overview → set the mode to "Full (strict)".**

> Never use **"Flexible"** mode — it makes hop #2 plain HTTP and exposes your
> `X-API-Key` in transit. Full (strict) + a Cloudflare Origin Certificate is the
> correct pairing for a credentialed API.

**Not using Cloudflare?** Point the `monitor` DNS record straight at the server
(no proxy) and instead issue a normal Let's Encrypt certificate for
`monitor.example.com` from your panel (e.g. Plesk's "SSL It!"). Everything in §5b
is the same; only the certificate source differs.

### 5b. Add the subdomain to your web server

**Plesk:**

1. **Websites & Domains → Add Subdomain** → `monitor.example.com`.
2. **SSL/TLS Certificates** for that subdomain → **Add** → paste the Cloudflare
   Origin certificate + private key from §5a → assign it to the subdomain.
3. **Apache & nginx Settings → Additional nginx directives:**

   ```nginx
   location / {
       proxy_pass http://127.0.0.1:8080;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
       proxy_set_header X-Forwarded-Proto $scheme;
   }
   ```

   Enable **Proxy mode** / disable "Smart static files processing" so nginx
   proxies everything to the monitor instead of handing it to Apache.

**Plain nginx (no panel):** add a server block for the subdomain — do **not** add
a second listener elsewhere or a new port; this is just another `server {}` on
the 443 nginx already runs:

```nginx
server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name monitor.example.com;

    ssl_certificate     /etc/ssl/cloudflare/monitor.example.com.pem;   # CF Origin cert
    ssl_certificate_key /etc/ssl/cloudflare/monitor.example.com.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Then `sudo nginx -t && sudo systemctl reload nginx`.

## 6. Firewall

**Nothing new to open.** Port 443 is already allowed for your existing sites, and
that's all the monitor uses. Just make sure 8080 is *not* exposed:

```sh
sudo ss -ltnp | grep ':8080'    # must show 127.0.0.1:8080 ONLY — never 0.0.0.0
```

If you run `ufw`, a typical baseline is simply 22 + 443:

```sh
sudo ufw allow 22/tcp
sudo ufw allow 443/tcp
sudo ufw enable
```

## 7. Verify it's live

```sh
export ADMIN_KEY=<your admin key from uptime-monitor.env>

# Liveness (unauthenticated):
curl https://monitor.example.com/health
# -> {"status":"ok","time":...}

# Authenticated admin read:
curl -H "X-API-Key: $ADMIN_KEY" https://monitor.example.com/sites
# -> []   (empty until you add sites)
```

If `/health` works but `/sites` returns 401, the key in `/etc/uptime-monitor.env`
doesn't match what you sent.

If you get a **502 Bad Gateway** from Cloudflare, the proxy reached your web
server but the monitor behind it didn't answer — check
`curl -s http://127.0.0.1:8080/health` on the box (is the service up?) and that
the `proxy_pass` target is `127.0.0.1:8080`. See Troubleshooting.

## 8. Add your first site and mint its consumer key

`generate_api_key: true` makes the server mint a random per-site key and return
it **once** in the response. Save it — only its SHA-256 is stored.

```sh
curl -X POST https://monitor.example.com/sites \
  -H "X-API-Key: $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "My API",
        "url": "https://api.example.com/health",
        "interval_seconds": 60,
        "generate_api_key": true,
        "checks": {
          "http":    {"enabled": true},
          "latency": {"enabled": true, "max_ms": 1500},
          "content": {"enabled": true, "must_not_contain": ["Exception"]},
          "ssl":     {"enabled": true, "warn_days": 14},
          "dns":     {"enabled": true}
        }
      }'
# -> response contains "api_key": "<save this once>"
```

Hand that per-site key (not the admin key) to the consuming system:

```sh
curl -H "X-API-Key: $MY_API_SITE_KEY" \
  https://monitor.example.com/sites/my-api/uptime

curl -H "X-API-Key: $MY_API_SITE_KEY" \
  'https://monitor.example.com/sites/my-api/metrics?window=24h'
```

Within a minute or two the check count will climb and an uptime percentage
appears.

### Rotating a site key

Takes effect immediately, no restart. The old key stops working at once:

```sh
curl -X PUT https://monitor.example.com/sites/my-api \
  -H "X-API-Key: $ADMIN_KEY" \
  -d '{"name":"My API","url":"https://api.example.com/health","generate_api_key":true}'
```

## 9. Browser dashboards (CORS)

If a browser app on another origin needs to call this API directly, set the
allowed origin(s) with `-cors-origins` on the `ExecStart` line (comma-separated;
empty disables CORS). Server-to-server consumers don't need this — CORS is a
browser-only mechanism.

```
ExecStart=/opt/uptime-monitor/monitor \
  -data /var/lib/uptime-monitor \
  -addr 127.0.0.1:8080 \
  -cors-origins https://portal.example.com
```

Origins are matched exactly (scheme included), and the API never uses `*` because
it authenticates with the `X-API-Key` header. See the `-cors-origins` flag in the
main [README](../README.md#flags).

## 10. Backups

All state lives in `/var/lib/uptime-monitor` (`registry.db` plus one `*.db` per
site). WAL mode is on, so copy the `.db`, `.db-wal`, and `.db-shm` files together
with the service briefly stopped:

```sh
sudo systemctl stop uptime-monitor
sudo tar czf /backup/uptime-$(date +%F).tgz -C /var/lib uptime-monitor
sudo systemctl start uptime-monitor
```

Sizing for reference: ~30 MB per site per year at one check/minute
(see [STORAGE.md](STORAGE.md)).

## 11. Upgrades

```sh
make linux && scp monitor youruser@SERVER_IP:/tmp/monitor
sudo systemctl stop uptime-monitor
sudo mv /tmp/monitor /opt/uptime-monitor/monitor
sudo chown uptime:uptime /opt/uptime-monitor/monitor
sudo chmod 750 /opt/uptime-monitor/monitor
sudo systemctl start uptime-monitor
```

Schema changes apply automatically on start via versioned migrations
(`PRAGMA user_version`), so no manual database steps are needed.

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| Service restarts, `status=203/EXEC` | Wrong-architecture or non-Linux binary (e.g. ran both `make` targets so the arm64 build won, or `make build` gave a macOS binary), or the binary isn't executable. Run `file /opt/uptime-monitor/monitor` + `uname -m`, rebuild with the matching target, redeploy |
| `/health` unreachable | Service down (`systemctl status uptime-monitor`) or proxy misconfigured |
| **Cloudflare 502 Bad Gateway** | Proxy reached the web server but the monitor didn't answer. Check `curl http://127.0.0.1:8080/health` on the box and that `proxy_pass` targets `127.0.0.1:8080` |
| **Cloudflare 526 / SSL errors** | Origin cert not trusted under "Full (strict)" — install a Cloudflare **Origin Certificate**, or drop to "Full" temporarily |
| Existing sites go offline after setup | A second proxy grabbed 443. Stop/disable it (`systemctl disable --now caddy`) and let your existing web server reclaim 443 |
| 401 on every authed call | Key mismatch between client and `/etc/uptime-monitor.env` |
| 401 on `/sites/{id}/*` with a site key | Wrong site's key — a per-site key only reads its own site |
| Browser `fetch` blocked by CORS | Origin not in `-cors-origins` (exact scheme+host match), or flag unset |
| Site permanently down, error mentions "blocked connection to non-public address" | `-block-private-targets` is on but the target is internal — remove the flag |
| `unable to open database file: out of memory (14)` | Misleading text — SQLite code `14` is `CANTOPEN`, not OOM. The data dir is missing or not writable by the `uptime` user. Use `StateDirectory=` (step 4) so systemd creates/owns it; or `chown -R uptime:uptime /var/lib/uptime-monitor` |
| `permission denied` on start | `/var/lib/uptime-monitor` not owned by the `uptime` user |
