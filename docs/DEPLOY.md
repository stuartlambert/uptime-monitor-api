# Deployment guide

Step-by-step runbook for taking uptime-monitor live on a Linux server, with the
monitor bound to loopback behind a TLS-terminating reverse proxy. Your
data-consuming systems talk to the proxy over HTTPS using per-site API keys.

**Assumes:** a fresh Ubuntu/Debian server, and a DNS A record
(`monitor.example.com`) pointing at it.

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

**Windows (PowerShell):**

```powershell
cd C:\Users\Stuar\Projects\uptime-monitor
$env:CGO_ENABLED=0; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -ldflags="-s -w" -o monitor ./cmd/monitor
```

**macOS / Linux (or anywhere with make):**

```sh
make linux          # -> ./monitor  (linux/amd64)
make linux-arm64    # ARM servers (Graviton, Ampere, Pi)
```

## 2. Copy the binary and env file to the server

```sh
scp monitor youruser@SERVER_IP:/tmp/monitor
scp uptime-monitor.env youruser@SERVER_IP:/tmp/uptime-monitor.env
```

## 3. Provision the server

Run as a dedicated unprivileged user with its own data directory:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin uptime
sudo mkdir -p /opt/uptime-monitor /var/lib/uptime-monitor

sudo mv /tmp/monitor /opt/uptime-monitor/monitor
sudo chmod 750 /opt/uptime-monitor/monitor

# Env file holds the admin key: root-only, 0600.
sudo mv /tmp/uptime-monitor.env /etc/uptime-monitor.env
sudo chown root:root /etc/uptime-monitor.env
sudo chmod 600 /etc/uptime-monitor.env

sudo chown -R uptime:uptime /opt/uptime-monitor /var/lib/uptime-monitor
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
ReadWritePaths=/var/lib/uptime-monitor
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

## 5. TLS reverse proxy

The API speaks plain HTTP and the key travels in a header, so TLS is mandatory
before it leaves the host.

### Option A — Caddy (automatic TLS, recommended)

```sh
sudo apt install -y caddy
echo 'monitor.example.com {
    reverse_proxy 127.0.0.1:8080
}' | sudo tee /etc/caddy/Caddyfile
sudo systemctl restart caddy
```

Caddy provisions and renews a Let's Encrypt certificate automatically.

### Option B — nginx + certbot

```nginx
server {
    listen 443 ssl;
    server_name monitor.example.com;

    ssl_certificate     /etc/letsencrypt/live/monitor.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/monitor.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
    }
}
```

Then: `sudo certbot --nginx -d monitor.example.com`

## 6. Firewall

Expose only 443 (and 22 for SSH). The monitor's 8080 stays on loopback.

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

## 9. Backups

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

## 10. Upgrades

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
| `/health` unreachable | Service down (`systemctl status uptime-monitor`) or proxy misconfigured |
| 401 on every authed call | Key mismatch between client and `/etc/uptime-monitor.env` |
| 401 on `/sites/{id}/*` with a site key | Wrong site's key — a per-site key only reads its own site |
| Site permanently down, error mentions "blocked connection to non-public address" | `-block-private-targets` is on but the target is internal — remove the flag |
| `permission denied` on start | `/var/lib/uptime-monitor` not owned by the `uptime` user |
