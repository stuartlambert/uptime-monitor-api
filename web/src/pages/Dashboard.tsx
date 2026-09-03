import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useQueries, useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import type { Window } from '../api/types'
import { StatusPill } from '../components/StatusPill'
import { Sparkline } from '../components/Charts'
import { WindowPicker } from '../components/WindowPicker'
import { daysUntil, fmtAgo, fmtMs, fmtPercent, uptimeClass } from '../format'

export function Dashboard() {
  const [window, setWindow] = useState<Window>('24h')
  const navigate = useNavigate()

  // One request for the whole table. Polled so the page stays live without a
  // manual refresh; 30s is well inside the default 60s check interval.
  const overview = useQuery({
    queryKey: ['overview', window],
    queryFn: () => api.overview(window),
    refetchInterval: 30_000,
  })

  const sites = overview.data ?? []

  // Sparklines are a nice-to-have, so they load per site after the table is
  // already on screen rather than blocking it.
  const sparks = useQueries({
    queries: sites.map((row) => ({
      queryKey: ['series', row.site.id, window, 24],
      queryFn: () => api.series(row.site.id, window, 24),
      staleTime: 60_000,
      enabled: row.checks > 0,
    })),
  })

  const down = sites.filter((r) => r.site.enabled && r.up === false)
  const paused = sites.filter((r) => !r.site.enabled)

  return (
    <>
      <div className="page-head">
        <h1>Dashboard</h1>
        <span className="spacer" />
        <WindowPicker value={window} onChange={setWindow} />
        <Link className="btn primary" to="/site/new">Add site</Link>
      </div>

      {overview.error && (
        <div className="notice err" role="alert">
          <strong>Could not load sites</strong>
          {overview.error.message}
        </div>
      )}

      {down.length > 0 && (
        <div className="notice err" role="status">
          <strong>{down.length} site{down.length === 1 ? '' : 's'} down</strong>
          {down.map((r) => r.site.name).join(', ')}
        </div>
      )}

      {overview.isLoading ? (
        <div className="card"><div className="skeleton" style={{ height: 180 }} /></div>
      ) : sites.length === 0 ? (
        <div className="card">
          <div className="empty">
            <h3>No sites yet</h3>
            <p>Add the first site and checks begin within a minute.</p>
            <Link className="btn primary" to="/site/new">Add site</Link>
          </div>
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Site</th>
                <th>Status</th>
                <th style={{ textAlign: 'right' }}>Uptime</th>
                <th style={{ textAlign: 'right' }}>Avg</th>
                <th>Trend</th>
                <th>Last check</th>
                <th>Certificate</th>
              </tr>
            </thead>
            <tbody>
              {sites.map((row, i) => {
                const series = sparks[i]?.data
                const certDays = row.ssl_expires_at ? daysUntil(row.ssl_expires_at) : null
                return (
                  <tr key={row.site.id} className="clickable"
                      onClick={() => navigate(`/site/${row.site.id}`)}>
                    <td>
                      <Link to={`/site/${row.site.id}`} onClick={(e) => e.stopPropagation()}
                            style={{ fontWeight: 600, textDecoration: 'none' }}>
                        {row.site.name}
                      </Link>
                      <div className="subtle" style={{ fontSize: 12 }}>{row.site.url}</div>
                      {row.error && <div className="subtle" style={{ color: 'var(--crit)' }}>{row.error}</div>}
                    </td>
                    <td><StatusPill up={row.up} enabled={row.site.enabled} /></td>
                    <td className="num">
                      {row.checks === 0 ? (
                        <span className="subtle">—</span>
                      ) : (
                        <span className={uptimeClass(row.uptime_percent, row.checks) === 'crit' ? 'mono' : 'mono'}
                              style={{ color: `var(--${uptimeClass(row.uptime_percent, row.checks) || 'ink'})` }}>
                          {fmtPercent(row.uptime_percent)}
                        </span>
                      )}
                      <div className="subtle" style={{ fontSize: 11 }}>
                        {row.checks} check{row.checks === 1 ? '' : 's'}
                      </div>
                    </td>
                    <td className="num">{row.checks === 0 ? '—' : fmtMs(row.avg_ms)}</td>
                    <td>{series ? <Sparkline points={series.points} field="avg_ms" /> : <span className="subtle">—</span>}</td>
                    <td className="subtle">{fmtAgo(row.last_check_ts)}</td>
                    <td>
                      {certDays === null ? (
                        <span className="subtle">—</span>
                      ) : certDays < 0 ? (
                        <span className="pill down"><span className="dot" />Expired</span>
                      ) : certDays <= (row.site.checks.ssl.warn_days || 14) ? (
                        <span className="pill paused"><span className="dot" />{certDays}d</span>
                      ) : (
                        <span className="subtle mono">{certDays}d</span>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      {paused.length > 0 && (
        <p className="subtle" style={{ marginTop: 12 }}>
          {paused.length} site{paused.length === 1 ? ' is' : 's are'} paused and not being checked.
        </p>
      )}
    </>
  )
}
