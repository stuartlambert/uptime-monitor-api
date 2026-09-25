---
title: Uptime Monitor
updated: 2026-09-25
---

# Uptime Monitor

Website uptime monitoring for the PinkCrab estate: a Go daemon checks configured
sites on a schedule, stores results in per-site SQLite files, serves a REST API,
and emails alerts. A React admin UI sits in front of it. Stuart is the only user;
consuming systems read one site's data with a per-site key.

|  |  |
|---|---|
| Owner / client | Stuart's own (PinkCrab) |
| Domain | `monitor.pinkcrab.co.uk` |
| URL | <https://monitor.pinkcrab.co.uk> (UI), `/api/` (API) |
| Runs on | [IONOS VPS](../servers/ionos-vps/index.md), `/opt/uptime-monitor/monitor`, systemd unit `uptime-monitor` |
| Repo | [stuartlambert/uptime-monitor-api](https://github.com/stuartlambert/uptime-monitor-api) — checkout at `~/Projects/uptime-monitor-api` |
| Stack | Go 1.22 (`modernc.org/sqlite`, `x/crypto` bcrypt, `x/term`), SQLite, Vite + React + TypeScript with TanStack Query and React Router, Apache on Plesk |
| Login | see [access](access.md) |

## Run it locally

```bash
cd ~/Projects/uptime-monitor-api
UPTIME_API_KEY=devkey make run     # daemon on :8080, seeded from seed.example.json
make web-install                   # once
make web-dev                       # UI on :5173, proxying /api to 127.0.0.1:8080
```

The daemon refuses to start without an admin credential, hence `UPTIME_API_KEY`.
UI at <http://localhost:5173>, API at <http://localhost:8080/api/health>.

| Command |  |
|---|---|
| `make build` | binary for this machine |
| `make linux` | static linux/amd64 binary — what production runs |
| `make linux-arm64` | static linux/arm64 binary |
| `make test` / `make vet` | `go test ./...` / `go vet ./...` |
| `make web` | production UI build into `web/dist` |
| `make clean` | remove binaries and `web/dist` |

The daemon is a single static binary — the pure-Go SQLite driver means
`CGO_ENABLED=0`, so it cross-compiles with no C toolchain. The UI is a separate
build, deliberately not embedded in it, so shipping a UI change needs no daemon
restart.

## Ports

| Service | Local | Deployed | Notes |
|---|---|---|---|
| API daemon | `8080` | `127.0.0.1:8080` | loopback only; Apache proxies `/api/` to it |
| Vite dev server | `5173` | — | development only |

The daemon is never exposed directly. Public access is Cloudflare → Apache :443
→ `127.0.0.1:8080`.

## Monitored sites

Three, as of September 2026: `english-stamp`, `glynn-quelch` and `pinkcrab-ops`
(<https://ops.pinkcrab.co.uk>). The live list is the dashboard, or
`GET /api/sites`.
