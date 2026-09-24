---
title: Uptime Monitor
updated: 2026-09-24
---

Website uptime monitoring for the PinkCrab estate: a Go daemon that checks
configured sites on a schedule, stores results in per-site SQLite files, serves a
REST API, and emails alerts. A React admin UI sits in front of it.

| | |
|---|---|
| Admin UI | <https://monitor.pinkcrab.co.uk> (behind Cloudflare Access) |
| API | `https://monitor.pinkcrab.co.uk/api/` |
| Repo | `git@github.com:stuartlambert/uptime-monitor-api.git` |
| Local checkout | `~/Projects/uptime-monitor-api` |
| Host | [IONOS VPS](../servers/ionos-vps/index.md) `87.106.69.203`, reached as `ssh ionos-vps` |
| Daemon | systemd unit `uptime-monitor`, bound to `127.0.0.1:8080` |

## Stack

| Part | |
|---|---|
| Daemon | Go 1.22, single static binary (`CGO_ENABLED=0`) |
| Storage | SQLite via `modernc.org/sqlite` (pure Go, no cgo) |
| Other deps | `golang.org/x/crypto` (bcrypt), `golang.org/x/term` |
| UI | Vite + React + TypeScript, TanStack Query, React Router |
| Web server | Apache on Plesk, serving the SPA and proxying `/api/` |

The UI is a separate build, deliberately not embedded in the binary, so a UI
change needs no daemon restart.

## Run it locally

```bash
cd ~/Projects/uptime-monitor-api
make run                      # builds and starts with seed.example.json
```

The daemon refuses to start without an admin credential. For a local run:

```bash
UPTIME_API_KEY=devkey make run
```

UI against a local daemon, on <http://localhost:5173> (Vite proxies `/api` to
`127.0.0.1:8080`):

```bash
make web-install              # once
make web-dev
```

| Command | |
|---|---|
| `make build` | binary for this machine |
| `make linux` | static linux/amd64 binary (what production runs) |
| `make test` | `go test ./...` |
| `make vet` | `go vet ./...` |
| `make web` | production UI build into `web/dist` |
| `make clean` | remove binaries and `web/dist` |

## Pages

- [Access](access.md) — logging in, API keys, where the secrets are
- [API](api.md) — every endpoint
- [Features](features.md) — what it checks and when it alerts
- [Operations](ops.md) — deploying, config, logs, backups
- [FAQ](faq.md) — common tasks
