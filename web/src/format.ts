/** Shared formatting helpers. Timestamps from the API are unix seconds. */

export const fmtTime = (ts: number | null | undefined) =>
  ts ? new Date(ts * 1000).toLocaleString(undefined, {
    year: 'numeric', month: 'short', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  }) : '—'

export const fmtDate = (ts: number | null | undefined) =>
  ts ? new Date(ts * 1000).toLocaleDateString(undefined, {
    year: 'numeric', month: 'short', day: '2-digit',
  }) : '—'

/** Compact relative age, e.g. "12s ago", "4m ago". */
export function fmtAgo(ts: number | null | undefined): string {
  if (!ts) return 'never'
  const secs = Math.max(0, Math.floor(Date.now() / 1000) - ts)
  if (secs < 60) return `${secs}s ago`
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`
  return `${Math.floor(secs / 86400)}d ago`
}

/** Duration in seconds as a short human string. */
export function fmtDuration(secs: number | null | undefined): string {
  if (secs === null || secs === undefined) return 'ongoing'
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.floor(secs / 60)}m ${secs % 60}s`
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  return `${h}h ${m}m`
}

/** The API already rounds to 2dp; this only trims a trailing ".00". */
export const fmtPercent = (v: number) =>
  `${Number.isInteger(v) ? v : v.toFixed(2)}%`

export const fmtMs = (v: number) =>
  v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${Math.round(v)}ms`

/** Days until a unix timestamp, negative once past. */
export const daysUntil = (ts: number) =>
  Math.floor((ts - Date.now() / 1000) / 86400)

/** Severity band for an uptime percentage. */
export function uptimeClass(pct: number, checks: number): '' | 'ok' | 'warn' | 'crit' {
  if (checks === 0) return ''
  if (pct >= 99.5) return 'ok'
  if (pct >= 95) return 'warn'
  return 'crit'
}
