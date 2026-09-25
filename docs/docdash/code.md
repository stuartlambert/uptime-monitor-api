---
title: Code
updated: 2026-09-25
---

# Code

A Go daemon plus a separate React SPA. The daemon is a single static binary with
no cgo, so it cross-compiles anywhere; the UI is built independently and served
as static files.

## Map

```
uptime-monitor-api/
├── cmd/monitor/
│   ├── main.go              flags, wiring, HTTP server, graceful shutdown
│   └── adminuser.go         first-boot admin seed and -set-password
├── internal/
│   ├── config/config.go     SiteConfig, defaults, validation, slugify
│   ├── checker/checker.go   runs one tick of checks against one site
│   ├── scheduler/scheduler.go  one goroutine per site, on its interval
│   ├── storage/
│   │   ├── sqlite.go        connection pragmas, unique-violation detection
│   │   ├── migrate.go       ordered schema history for both database kinds
│   │   ├── registry.go      site configs in registry.db
│   │   ├── manager.go       opens and caches one SiteStore per site
│   │   ├── site.go          opens a per-site database
│   │   ├── tick.go          writes a check result, reconciles incidents
│   │   ├── query.go         status, uptime, metrics, incidents, errors, results
│   │   ├── series.go        bucketed uptime and latency for charts
│   │   ├── overview.go      one row per site for the dashboard
│   │   ├── admin.go         admin accounts
│   │   ├── session.go       server-side sessions
│   │   └── alerts.go        channels, rules, delivery log
│   ├── auth/
│   │   ├── auth.go          bcrypt, session tokens, timing-safe compare
│   │   └── ratelimit.go     login throttling, per username and per IP
│   ├── alerts/
│   │   ├── alerts.go        dispatcher: queue, workers, rules, dedupe, retry
│   │   └── smtp.go          the SMTP sender
│   └── api/
│       ├── api.go           routing, auth middleware, sites and data handlers
│       ├── auth.go          login, logout, me, password
│       └── alerts.go        alert channel and rule handlers
├── web/src/
│   ├── main.tsx             React root, query client
│   ├── App.tsx              routes and the signed-in shell
│   ├── api/client.ts        fetch wrapper, typed calls, 401 handling
│   ├── api/types.ts         TypeScript mirrors of the Go JSON shapes
│   ├── format.ts            shared number, duration and date formatting
│   ├── pages/               one file per screen
│   └── components/          charts, status pill, window picker
├── deploy/                  deploy script and vhost configs
└── docs/                    DEPLOY, ROLLOUT, STORAGE, and these pages
```

## Entry points

| What | Where |
|---|---|
| Daemon start | `cmd/monitor/main.go` — `main()` parses flags, `run()` does everything |
| HTTP routes | `internal/api/api.go` — `Handler()` mounts, `apiMux()` registers |
| Check loop | `internal/scheduler/scheduler.go` — `StartAll()`, then `runSite()` per site |
| One check | `internal/checker/checker.go` — `RunTick()` |
| Alert dispatch | `internal/alerts/alerts.go` — `NewDispatcher()`, `handle()` |
| Migrations | `internal/storage/migrate.go` — `applyMigrations()`, on every database open |
| Password reset | `cmd/monitor/adminuser.go` — `setPassword()`, exits without starting |
| UI | `web/src/main.tsx` → `web/src/App.tsx` |

## Follow a request

`GET /api/sites/english-stamp/uptime?window=24h` with a per-site key:

1. `internal/api/api.go` — `Handler()` matches `/api/`, strips the prefix, hands
   `/sites/english-stamp/uptime` to the inner mux.
2. `apiMux()` matches `GET /sites/{id}/uptime`, wrapped in `requireSiteRead()`.
3. `requireSiteRead()` loads the site from the registry — an unknown id 404s
   here — then accepts either an admin credential via `adminOK()`, or the
   presented key run through `config.HashAPIKey()` and compared against the
   site's stored hash with `keyMatches()`. Neither matching is a 401.
4. `handleUptime()` calls `storeFor()`, which resolves `{id}` through
   `storage.Manager.Get()` — `safeID()` there refuses any id that could escape
   the data directory.
5. `parseWindow()` turns `?window=24h` into a name and a cutoff timestamp.
6. `internal/storage/query.go` — `Uptime()` runs one aggregate query and rounds
   to 2dp.
7. `writeJSON()` sends it.

And one check, the other direction:

1. `scheduler.runSite()` fires on the site's ticker.
2. `checker.RunTick()` performs the HTTP request and the enabled checks, and
   returns a `storage.Tick`.
3. `storage.RecordTick()` writes the row and reconciles incidents in one
   transaction, returning a `Transition`.
4. Back in `runSite()`, **after the commit**, the registered `TransitionFunc`
   runs — in production that is `Dispatcher.Enqueue()`.
5. A dispatcher worker calls `handle()` → `applyRule()` → `ClaimDelivery()` →
   `deliver()` → `SMTPSender.Send()`.

## Conventions

- **Tests sit beside the code**, `_test.go` in the same package.
  `internal/alerts/integration_test.go` is the only one that wires real
  components together; the rest are unit-level.
- **Every SQL statement is parameterised.** The single `Sprintf` into SQL sets
  `PRAGMA user_version` from an integer in our own migration list, because a
  pragma cannot take a bound parameter.
- **One SQLite file per site**, plus `registry.db` for everything global. One
  site's history never touches another's.
- **Handlers are thin.** They parse, call one storage method, and write JSON.
  Anything else belongs in `internal/storage` or `internal/alerts`.
- **Auth middleware wraps at registration**, in `apiMux()` — `requireAdmin()` or
  `requireSiteRead()`. A route registered without one is unauthenticated, so
  check the wrapper when adding an endpoint.
- **Outbound work never happens inside a transaction.** `RecordTick()` returns
  what changed and the caller acts after the commit.
- **Secrets never serialise**: hash fields carry `json:"-"`, and a generated key
  is returned once from the handler that created it.
