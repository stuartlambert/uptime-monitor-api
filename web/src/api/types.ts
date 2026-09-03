// Types mirroring the Go JSON shapes in internal/storage and internal/config.
// Kept hand-written rather than generated: the API is small and stable, and a
// generator would be a second toolchain for the sake of ~150 lines.

export interface HTTPCheck {
  enabled: boolean
  expect_status: number[] | null
}
export interface LatencyCheck {
  enabled: boolean
  max_ms: number
}
export interface ContentCheck {
  enabled: boolean
  must_contain: string[] | null
  must_not_contain: string[] | null
}
export interface SSLCheck {
  enabled: boolean
  warn_days: number
}
export interface DNSCheck {
  enabled: boolean
}

export interface ChecksConfig {
  http: HTTPCheck
  latency: LatencyCheck
  content: ContentCheck
  ssl: SSLCheck
  dns: DNSCheck
}

export interface SiteConfig {
  id: string
  name: string
  url: string
  interval_seconds: number
  slow_interval_seconds: number
  enabled: boolean
  checks: ChecksConfig
  created_at: number
  updated_at: number
  /** Derived server-side; the key itself is never returned after creation. */
  has_api_key: boolean
  /** Present only in the response to a create/update that minted a key. */
  api_key?: string
}

export interface SiteOverview {
  site: SiteConfig
  /** null when the site has never been checked — not the same as false. */
  up: boolean | null
  last_check_ts: number | null
  ongoing_incident_id: number | null
  ssl_expires_at: number | null
  window: string
  checks: number
  failed: number
  uptime_percent: number
  avg_ms: number
  /** Set when this one site's data could not be read. */
  error?: string
}

export interface SiteStatus {
  monitoring_started_at: number
  last_check_ts: number | null
  up: boolean | null
  ongoing_incident_id: number | null
  ssl_expires_at: number | null
  ssl_last_checked: number | null
}

export interface Uptime {
  window: string
  checks: number
  successful: number
  failed: number
  uptime_percent: number
}

export interface Metrics {
  window: string
  checks: number
  successful: number
  failed: number
  avg_ms: number
  p50_ms: number
  p95_ms: number
  p99_ms: number
}

export interface SeriesPoint {
  ts: number
  checks: number
  successful: number
  uptime_percent: number
  avg_ms: number
  p95_ms: number
}

export interface Series {
  window: string
  from: number
  to: number
  bucket_seconds: number
  points: SeriesPoint[]
}

export interface Incident {
  id: number
  started_at: number
  resolved_at: number | null
  cause: string
  duration_seconds: number | null
}

export interface ErrorRow {
  id: number
  ts: number
  check_type: string
  message: string
}

export interface ResultRow {
  id: number
  ts: number
  up: boolean
  status_code: number
  response_ms: number
  failed_checks?: string
  error?: string
}

export type AlertKind = 'site_down' | 'site_recovered' | 'ssl_expiring'

export interface AlertChannel {
  id: number
  name: string
  type: string
  target: string
  enabled: boolean
  created_at: number
  updated_at: number
}

export interface AlertRule {
  id: number
  /** null means every site, including ones added later. */
  site_id: string | null
  channel_id: number
  kind: AlertKind
  enabled: boolean
  confirm_after: number
  created_at: number
  updated_at: number
}

export interface AlertDelivery {
  id: number
  dedupe_key: string
  site_id: string
  kind: string
  channel_id: number
  incident_id: number | null
  subject: string
  status: 'pending' | 'sent' | 'failed'
  attempts: number
  error: string | null
  created_at: number
  sent_at: number | null
}

export interface Me {
  username: string | null
  auth: 'session' | 'api_key'
}

export type Window = '1h' | '24h' | '7d' | '30d' | 'all'
export const WINDOWS: Window[] = ['1h', '24h', '7d', '30d', 'all']
