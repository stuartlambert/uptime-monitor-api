import type {
  AlertChannel, AlertDelivery, AlertRule, ErrorRow, Incident, Me, Metrics,
  ResultRow, Series, SiteConfig, SiteOverview, SiteStatus, Uptime,
} from './types'

/** Thrown for any non-2xx response, carrying the server's own message. */
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'ApiError'
  }
}

/** Raised on 401 so the router can send the user to the login screen. */
export class UnauthorizedError extends ApiError {
  constructor(message = 'Not signed in') {
    super(401, message)
    this.name = 'UnauthorizedError'
  }
}

const BASE = '/api'

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    ...init,
    headers: {
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...init?.headers,
    },
    // Same-origin deployment, so the session cookie rides along by default.
    // Stated explicitly because it is the whole authentication mechanism.
    credentials: 'same-origin',
  })

  if (res.status === 204) return undefined as T

  const text = await res.text()
  let payload: unknown = null
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      // A non-JSON body means something other than the API answered — most
      // likely the reverse proxy. Surface it rather than a parse error.
      if (!res.ok) throw new ApiError(res.status, text.slice(0, 200))
    }
  }

  if (!res.ok) {
    const message =
      (payload as { error?: string } | null)?.error ?? `Request failed (${res.status})`
    if (res.status === 401) throw new UnauthorizedError(message)
    throw new ApiError(res.status, message)
  }
  return payload as T
}

const qs = (params: Record<string, string | number | undefined>) => {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `?${s}` : ''
}

export const api = {
  // --- auth ---
  login: (username: string, password: string) =>
    request<{ username: string; expires_at: number }>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  me: () => request<Me>('/auth/me'),
  changePassword: (current_password: string, new_password: string) =>
    request<{ status: string }>('/auth/password', {
      method: 'POST',
      body: JSON.stringify({ current_password, new_password }),
    }),

  // --- sites ---
  overview: (window: string) =>
    request<SiteOverview[]>(`/sites/overview${qs({ window })}`),
  listSites: () => request<SiteConfig[]>('/sites'),
  getSite: (id: string) => request<SiteConfig>(`/sites/${encodeURIComponent(id)}`),
  createSite: (body: unknown) =>
    request<SiteConfig>('/sites', { method: 'POST', body: JSON.stringify(body) }),
  updateSite: (id: string, body: unknown) =>
    request<SiteConfig>(`/sites/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  deleteSite: (id: string, purge: boolean) =>
    request<void>(`/sites/${encodeURIComponent(id)}${qs({ purge: purge ? 'true' : undefined })}`, {
      method: 'DELETE',
    }),

  // --- site data ---
  status: (id: string) => request<SiteStatus>(`/sites/${encodeURIComponent(id)}/status`),
  uptime: (id: string, window: string) =>
    request<Uptime>(`/sites/${encodeURIComponent(id)}/uptime${qs({ window })}`),
  metrics: (id: string, window: string) =>
    request<Metrics>(`/sites/${encodeURIComponent(id)}/metrics${qs({ window })}`),
  series: (id: string, window: string, buckets: number) =>
    request<Series>(`/sites/${encodeURIComponent(id)}/series${qs({ window, buckets })}`),
  incidents: (id: string, limit = 25) =>
    request<Incident[]>(`/sites/${encodeURIComponent(id)}/incidents${qs({ limit })}`),
  errors: (id: string, limit = 50) =>
    request<ErrorRow[]>(`/sites/${encodeURIComponent(id)}/errors${qs({ limit })}`),
  results: (id: string, limit = 50) =>
    request<ResultRow[]>(`/sites/${encodeURIComponent(id)}/results${qs({ limit })}`),

  // --- alerts ---
  channels: () => request<AlertChannel[]>('/alerts/channels'),
  createChannel: (body: unknown) =>
    request<AlertChannel>('/alerts/channels', { method: 'POST', body: JSON.stringify(body) }),
  updateChannel: (id: number, body: unknown) =>
    request<AlertChannel>(`/alerts/channels/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteChannel: (id: number) => request<void>(`/alerts/channels/${id}`, { method: 'DELETE' }),
  testChannel: (id: number) =>
    request<{ status: string; target: string }>(`/alerts/channels/${id}/test`, { method: 'POST' }),

  rules: () => request<AlertRule[]>('/alerts/rules'),
  createRule: (body: unknown) =>
    request<AlertRule>('/alerts/rules', { method: 'POST', body: JSON.stringify(body) }),
  updateRule: (id: number, body: unknown) =>
    request<AlertRule>(`/alerts/rules/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteRule: (id: number) => request<void>(`/alerts/rules/${id}`, { method: 'DELETE' }),

  deliveries: (limit = 50) => request<AlertDelivery[]>(`/alerts/deliveries${qs({ limit })}`),
}
