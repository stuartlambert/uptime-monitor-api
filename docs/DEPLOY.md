# Production deployment runbook

Target: a Linux server (systemd) with the monitor behind a TLS-terminating
reverse proxy. Your data-consuming system talks to the proxy over HTTPS with an
API key.

## 1. Build the binary

On any machine with Go 1.22+ (the binary is static, no runtime deps):

```sh
make linux            # -> ./monitor  (linux/amd64)
# or: make linux-arm64 for ARM servers
```

Copy it to the server:

```sh
scp monitor youruser@server:/tmp/monitor
```

## 2. Provision the server

Run as a dedicated unprivileged user with its own data directory:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin uptime
sudo mkdir -p /opt/uptime-monitor /var/lib/uptime-monitor
sudo mv /tmp/monitor /opt/uptime-monitor/monitor
sudo chown -R uptime:uptime /opt/uptime-monitor /var/lib/uptime-monitor
sudo chmod 750 /opt/uptime-monitor/monitor
```

## 3. Generate an API key

```sh
openssl rand -hex 32          # copy the output
```

Store it in an environment file readable only by root:

```sh
sudo tee /etc/uptime-monitor.env >/dev/null <<'EOF'
UPTIME_API_KEY=paste-the-generated-key-here
EOF
sudo chmod 600 /etc/uptime-monitor.env
```

## 4. systemd unit

`/etc/systemd/system/uptime-monitor.service`:

```ini
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
```

Notes:
- Bind to `127.0.0.1:8080` so the monitor is reachable only via the local
  reverse proxy, never directly from the network.
- The key is read from `UPTIME_API_KEY` (no need to put it on the command line,
  where it would show in `ps`).
- Add `-block-private-targets` to the `ExecStart` line **if** you do not intend
  to monitor internal/private hosts (SSRF guard).

Enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now uptime-monitor
sudo systemctl status uptime-monitor
journalctl -u uptime-monitor -f      # live logs
```

## 5. TLS reverse proxy

The API speaks plain HTTP and the key travels in a header, so TLS is mandatory
before it leaves the host.

### Option A — Caddy (automatic TLS)

`/etc/caddy/Caddyfile`:

```
monitor.example.com {
    reverse_proxy 127.0.0.1:8080
}
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

Obtain the cert with `sudo certbot --nginx -d monitor.example.com`.

## 6. Firewall

Expose only 443 (and 22 for SSH). The monitor's 8080 stays on loopback.

```sh
sudo ufw allow 22/tcp
sudo ufw allow 443/tcp
sudo ufw enable
```

## 7. Add sites

Create sites over the API with the **admin** key. Add `"generate_api_key": true`
to mint a per-site read key — it is returned once in the response; hand that key
(not the admin key) to the system that consumes this site's data.

```sh
curl -sS https://monitor.example.com/sites \
  -H "X-API-Key: $UPTIME_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "My API",
        "url": "https://api.example.com/health",
        "interval_seconds": 60,
        "generate_api_key": true,
        "checks": {
          "http": {"enabled": true},
          "latency": {"enabled": true, "max_ms": 1500},
          "content": {"enabled": true, "must_not_contain": ["Exception"]},
          "ssl": {"enabled": true, "warn_days": 14},
          "dns": {"enabled": true}
        }
      }'
# -> response contains "api_key": "<save this once>"
```

The consuming system then reads only its own site:

```sh
curl -sS https://monitor.example.com/sites/my-api/uptime \
  -H "X-API-Key: $MY_API_SITE_KEY"
```

Rotate a site key any time with `PUT /sites/{id}` and `"generate_api_key": true`
— the old key stops working immediately, no restart needed.

## 8. Verify it's live

```sh
# Monitor liveness (unauthenticated):
curl -sS https://monitor.example.com/health

# Authenticated read your consuming system will use:
curl -sS https://monitor.example.com/sites/my-api/uptime \
  -H "X-API-Key: $UPTIME_API_KEY"
```

You should see a growing check count and an uptime percentage within a couple of
minutes.

## 9. Backups

All state is in `/var/lib/uptime-monitor` (registry.db + per-site *.db). Because
WAL mode is on, back up with the SQLite backup API or copy the `.db`, `.db-wal`,
and `.db-shm` files together while the service is briefly stopped:

```sh
sudo systemctl stop uptime-monitor
sudo tar czf /backup/uptime-$(date +%F).tgz -C /var/lib uptime-monitor
sudo systemctl start uptime-monitor
```

## 10. Upgrades

```sh
make linux && scp monitor server:/tmp/monitor
sudo systemctl stop uptime-monitor
sudo mv /tmp/monitor /opt/uptime-monitor/monitor
sudo chown uptime:uptime /opt/uptime-monitor/monitor
sudo systemctl start uptime-monitor
```

Schema changes are applied automatically on start via versioned migrations.
