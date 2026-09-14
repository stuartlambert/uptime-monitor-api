import { useState } from 'react'
import type { ReactNode } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import { fmtDuration, fmtTime } from '../format'

// The API clamps limit to 10,000; failures are rare, so one large fetch covers
// the whole history and we page through it in the browser.
const MAX = 10_000
const PAGE = 50

/** Full, paginated history of every incident and error for one site. */
export function SiteHistory() {
  const { id = '' } = useParams()
  const nowTs = Math.floor(Date.now() / 1000)

  const site = useQuery({ queryKey: ['site', id], queryFn: () => api.getSite(id) })
  const incidents = useQuery({ queryKey: ['incidents', id, 'all'], queryFn: () => api.incidents(id, MAX) })
  const errors = useQuery({ queryKey: ['errors', id, 'all'], queryFn: () => api.errors(id, MAX) })

  return (
    <>
      <div className="page-head">
        <div>
          <h1>History</h1>
          <Link className="subtle" to={`/site/${id}`}>← {site.data?.name ?? id}</Link>
        </div>
      </div>

      <Section
        title="Incidents"
        query={incidents}
        empty="No incidents recorded."
        head={<tr><th>Started</th><th>Resolved</th><th>Down for</th><th>Cause</th></tr>}
        row={(inc) => {
          const ongoing = inc.resolved_at === null
          const downForS = ongoing ? Math.max(0, nowTs - inc.started_at) : inc.duration_seconds
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
        }}
      />

      <Section
        title="Errors"
        query={errors}
        empty="No errors recorded."
        head={<tr><th>When</th><th>Check</th><th>Message</th></tr>}
        row={(row) => (
          <tr key={row.id}>
            <td className="mono" style={{ fontSize: 12 }}>{fmtTime(row.ts)}</td>
            <td><span className="tag">{row.check_type || '—'}</span></td>
            <td className="subtle">{row.message}</td>
          </tr>
        )}
      />
    </>
  )
}

interface SectionProps<T> {
  title: string
  query: { data?: T[]; isLoading: boolean; error: unknown }
  empty: string
  head: ReactNode
  row: (item: T) => ReactNode
}

/** A titled card holding one paginated table. */
function Section<T>({ title, query, empty, head, row }: SectionProps<T>) {
  const [page, setPage] = useState(0)
  const all = query.data ?? []
  const pages = Math.max(1, Math.ceil(all.length / PAGE))
  const clamped = Math.min(page, pages - 1)
  const start = clamped * PAGE
  const slice = all.slice(start, start + PAGE)

  return (
    <div className="card">
      <div className="card-head">
        <h2>{title}</h2>
        <span className="spacer" />
        {all.length > 0 && (
          <span className="subtle" style={{ fontSize: 12 }}>
            {start + 1}–{start + slice.length} of {all.length}
          </span>
        )}
      </div>

      {query.isLoading ? (
        <div className="skeleton" style={{ height: 120 }} />
      ) : query.error ? (
        <div className="notice err">{(query.error as Error).message}</div>
      ) : all.length === 0 ? (
        <p className="subtle" style={{ margin: 0 }}>{empty}</p>
      ) : (
        <>
          <div className="table-wrap">
            <table style={{ minWidth: 360 }}>
              <thead>{head}</thead>
              <tbody>{slice.map(row)}</tbody>
            </table>
          </div>
          {pages > 1 && (
            <div className="actions" style={{ marginTop: 12, alignItems: 'center' }}>
              <button className="small" disabled={clamped === 0} onClick={() => setPage(clamped - 1)}>
                ← Newer
              </button>
              <span className="subtle" style={{ fontSize: 12 }}>Page {clamped + 1} of {pages}</span>
              <button className="small" disabled={clamped >= pages - 1} onClick={() => setPage(clamped + 1)}>
                Older →
              </button>
            </div>
          )}
        </>
      )}
    </div>
  )
}
