# Storage design & sizing

## Per-site databases

Every monitored site gets its own SQLite file at `data/<id>.db`. The registry of
site configs lives separately in `data/registry.db`. One site's history never
touches another's, and a site can be archived by copying or deleting a single
file (`DELETE /sites/{id}?purge=true` removes it).

## Lean schema

The design writes **one row per check tick** into `check_results`:

| column          | notes                                             |
|-----------------|---------------------------------------------------|
| `id`            | INTEGER PRIMARY KEY — rowid, assigned in time order |
| `ts`            | unix seconds                                      |
| `up`            | 1 if all enabled checks passed                    |
| `status_code`   | HTTP status (0 if the request failed)             |
| `response_ms`   | request duration                                  |
| `failed_checks` | comma-list of failed check types (NULL when up)   |
| `error`         | primary error message (NULL when up)              |

A single HTTP request per tick feeds the HTTP-status, latency, and content
checks. SSL and DNS run on a **slow cadence** (`slow_interval_seconds`, default
hourly) because their values change on the order of days — this avoids a second
row/probe per minute.

`incidents` and `errors` only accrue rows on failures, so they stay tiny.
`site_state` is a single rolled-up row (current up/down, open incident, SSL
expiry, monitoring start time) so the hot API endpoints don't scan history.

### No timestamp index (by design)

Rows are appended in time order, so `id` (rowid) already orders by time. We
deliberately omit an index on `ts` to keep the file small. Range queries filter
`WHERE ts >= ?`, which is a sequential scan — for a per-minute site that's ~525k
tiny rows/year, a few tens of milliseconds, fine for an occasional pull.

If you later need faster range queries, one line adds it back:

```sql
CREATE INDEX idx_check_results_ts ON check_results(ts);
```

## Sizing (5 sites, per-minute, 12 months)

- **525,600 checks/site/year**, 2.6M across 5 sites.
- Lean schema, ~55–60 bytes/row on disk including SQLite overhead:
  **~30 MB/site → ~150 MB total for the year.**
- Dropping the (already omitted) ts index keeps it nearer **~90 MB**.
- `errors` + `incidents` add well under 1 MB/site/year even at 1% failure.

At this scale retention isn't necessary. If you ever want to cap long-term
growth, a nightly rollup of per-minute rows into hourly aggregates after 30–90
days cuts stored volume ~60× while preserving uptime %, latency, and incidents.
