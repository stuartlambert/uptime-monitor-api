import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import type { Window } from '../api/types'
import { StatusPill } from '../components/StatusPill'
import { LineChart, UptimeBars } from '../components/Charts'
import { WindowPicker } from '../components/WindowPicker'
import { daysUntil, fmtAgo, fmtDuration, fmtMs, fmtPercent, fmtTime, uptimeClass } from '../format'

export function SiteDetail() {
  const { id = '' } = useParams()
  const [window, setWindow] = useState<Window>('24h')
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [purge, setPurge] = useState(false)
  const [nowTs, setNowTs] = useState(() => Math.floor(Date.now() / 1000))
  const qc = useQueryClient()
  const navigate = useNavigate()

  const site = useQuery({ queryKey: ['site', id], queryFn: () => api.getSite(id) })
  const status = useQuery({
    queryKey: ['status', id], queryFn: () => api.status(id), refetchInterval: 30_000,
  })
  const metrics = useQuery({ queryKey: ['metrics', id, window], queryFn: () => api.metrics(id, window) })
  const uptime = useQuery({ queryKey: ['uptime', id, window], queryFn: () => api.uptime(id, window) })
  const series = useQuery({ queryKey: ['series', id, window, 60], queryFn: () => api.series(id, window, 60) })
  const incidents = useQuery({
    queryKey: ['incidents', id], queryFn: () => api.incidents(id, 25), refetchInterval: 30_000,
  })
  const errors = useQuery({ queryKey: ['errors', id], queryFn: () => api.errors(id, 50) })

  // While an incident is open, tick a 1s clock so the ongoing outage timer counts up.
  const hasOngoingIncident = (incidents.data ?? []).some((i) => i.resolved_at === null)
  useEffect(() => {
    if (!hasOngoingIncident) return
    const t = setInterval(() => setNowTs(Math.floor(Date.now() / 1000)), 1000)
    return () => clearInterval(t)
  }, [hasOngoingIncident])

  const del = useMutation({
    mutationFn: () => api.deleteSite(id, purge),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['overview'] })
      navigate('/', { replace: true })
    },
  })

  if (site.error) {
    return (
      <div className="notice err" role="alert">
        <strong>Could not load this site</strong>
        {site.error.message}
      </div>
    )
  }

  const cfg = site.data
  const certDays = status.data?.ssl_expires_at ? daysUntil(status.data.ssl_expires_at) : null
  const u = uptime.data
  const m = metrics.data

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{cfg?.name ?? id}</h1>
          <a className="subtle" href={cfg?.url} target="_blank" rel="noreferrer noopener">{cfg?.url}</a>
        </div>
        <span className="spacer" />
        <StatusPill up={status.data?.up ?? null} enabled={cfg?.enabled ?? true} />
        <WindowPicker value={window} onChange={setWindow} />
        <Link className="btn" to={`/site/${id}/edit`}>Edit</Link>
      </div>

      {status.data?.ongoing_incident_id && (
        <div className="notice err" role="status">
          <strong>Currently down</strong>
          An incident is open. Recent errors are listed below.
        </div>
      )}
      {certDays !== null && certDays <= (cfg?.checks.ssl.warn_days || 14) && (
        <div className={`notice ${certDays < 0 ? 'err' : 'warn'}`}>
          <strong>{certDays < 0 ? 'Certificate expired' : `Certificate expires in ${certDays} day${certDays === 1 ? '' : 's'}`}</strong>
          {fmtTime(status.data?.ssl_expires_at)}
        </div>
      )}

      <div className="grid cols-4" style={{ marginBottom: 16 }}>
        <div className="stat">
          <div className="label">Uptime · {window}</div>
          <div className={`value ${u ? uptimeClass(u.uptime_percent, u.checks) : ''}`}>
            {u && u.checks > 0 ? fmtPercent(u.uptime_percent) : '—'}
          </div>
          <div className="sub">{u ? `${u.failed} failed of ${u.checks}` : ''}</div>
        </div>
        <div className="stat">
          <div className="label">Average</div>
          <div className="value">{m && m.checks > 0 ? fmtMs(m.avg_ms) : '—'}</div>
          <div className="sub">successful checks only</div>
        </div>
        <div className="stat">
          <div className="label">p95</div>
          <div className="value">{m && m.checks > 0 ? fmtMs(m.p95_ms) : '—'}</div>
          <div className="sub">p99 {m && m.checks > 0 ? fmtMs(m.p99_ms) : '—'}</div>
        </div>
        <div className="stat">
          <div className="label">Last check</div>
          <div className="value" style={{ fontSize: '1.15rem' }}>{fmtAgo(status.data?.last_check_ts)}</div>
          <div className="sub">every {cfg?.interval_seconds ?? '—'}s</div>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Uptime</h2>
          <span className="spacer" />
          <span className="subtle" style={{ fontSize: 12 }}>
            {series.data ? `${series.data.points.length} periods of ${series.data.bucket_seconds}s` : ''}
          </span>
        </div>
        {series.data ? <UptimeBars points={series.data.points} /> : <div className="skeleton" style={{ height: 34 }} />}
        <p className="subtle" style={{ fontSize: 12, marginTop: 8, marginBottom: 0 }}>
          Grey means no checks ran in that period.
        </p>
      </div>

      <div className="card">
        <div className="card-head"><h2>Response time</h2></div>
        {series.data
          ? <LineChart points={series.data.points} field="p95_ms" label="p95 latency" />
          : <div className="skeleton" style={{ height: 160 }} />}
      </div>

      <div className="grid cols-2" style={{ marginTop: 16 }}>
        <div className="card" style={{ marginTop: 0 }}>
          <div className="card-head"><h2>Incidents</h2></div>
          {(incidents.data ?? []).length === 0 ? (
            <p className="subtle" style={{ margin: 0 }}>No incidents recorded.</p>
          ) : (
            <>
              <div className="table-wrap">
                <table style={{ minWidth: 360 }}>
                  <thead><tr><th>Started</th><th>Resolved</th><th>Down for</th><th>Cause</th></tr></thead>
                  <tbody>
                    {incidents.data!.map((inc) => {
                      const ongoing = inc.resolved_at === null
                      const downForS = ongoing
                        ? Math.max(0, nowTs - inc.started_at)
                        : inc.duration_seconds
                      return (
                        <tr key={inc.id}>
                          <td className="mono" style={{ fontSize: 12 }}>{fmtTime(inc.started_at)}</td>
                          <td className="mono" style={{ fontSize: 12 }}>
                            {ongoing
                              ? <span className="pill down"><span className="dot" />Ongoing</span>
                              : fmtTime(inc.resolved_at)}
                          </td>
                          <td className="mono" style={ongoing ? { color: 'var(--crit)' } : undefined}>{fmtDuration(downForS)}</td>
                          <td className="subtle">{inc.cause || '—'}</td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
              <p className="subtle" style={{ fontSize: 12, marginTop: 8, marginBottom: 0 }}>
                Each row is one outage; the site recovered between separate rows. “Down for” is the total time offline.
              </p>
            </>
          )}
        </div>

        <div className="card" style={{ marginTop: 0 }}>
          <div className="card-head"><h2>Recent errors</h2></div>
          {(errors.data ?? []).length === 0 ? (
            <p className="subtle" style={{ margin: 0 }}>No errors recorded.</p>
          ) : (
            <div className="table-wrap">
              <table style={{ minWidth: 320 }}>
                <thead><tr><th>When</th><th>Check</th><th>Message</th></tr></thead>
                <tbody>
                  {errors.data!.slice(0, 25).map((row) => (
                    <tr key={row.id}>
                      <td className="mono" style={{ fontSize: 12 }}>{fmtTime(row.ts)}</td>
                      <td><span className="tag">{row.check_type || '—'}</span></td>
                      <td className="subtle">{row.message}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      <div className="card" style={{ marginTop: 'var(--gap)' }}>
        <div className="card-head"><h2>Danger zone</h2></div>
        {del.error && <div className="notice err">{del.error.message}</div>}
        {!confirmDelete ? (
          <div className="actions">
            <button className="danger" onClick={() => setConfirmDelete(true)}>Delete this site</button>
            <span className="subtle">Stops checks and removes the configuration.</span>
          </div>
        ) : (
          <>
            <div className="notice warn">
              <strong>Delete “{cfg?.name}”?</strong>
              This cannot be undone.
            </div>
            <div className="check">
              <input id="purge" type="checkbox" checked={purge} onChange={(e) => setPurge(e.target.checked)} />
              <div className="body">
                <strong>Also delete its collected history</strong>
                <span className="hint">Removes the site’s database file — every check result, incident, and error.</span>
              </div>
            </div>
            <div className="actions">
              <button className="danger" onClick={() => del.mutate()} disabled={del.isPending}>
                {del.isPending ? 'Deleting…' : 'Delete permanently'}
              </button>
              <button onClick={() => { setConfirmDelete(false); setPurge(false) }}>Cancel</button>
            </div>
          </>
        )}
      </div>
    </>
  )
}
