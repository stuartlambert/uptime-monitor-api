import { useEffect, useState, type FormEvent } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'

interface Props {
  mode: 'create' | 'edit'
}

/** Form state is kept as strings so partially-typed numbers don't fight the
 *  input. It is converted once, on submit. */
interface FormState {
  name: string
  url: string
  interval_seconds: string
  slow_interval_seconds: string
  enabled: boolean
  httpEnabled: boolean
  expectStatus: string
  latencyEnabled: boolean
  latencyMaxMs: string
  contentEnabled: boolean
  mustContain: string
  mustNotContain: string
  sslEnabled: boolean
  sslWarnDays: string
  dnsEnabled: boolean
  generateKey: boolean
}

const blank: FormState = {
  name: '', url: '', interval_seconds: '60', slow_interval_seconds: '3600', enabled: true,
  httpEnabled: true, expectStatus: '',
  latencyEnabled: false, latencyMaxMs: '1500',
  contentEnabled: false, mustContain: '', mustNotContain: '',
  sslEnabled: true, sslWarnDays: '14',
  dnsEnabled: true,
  generateKey: false,
}

const splitList = (s: string) =>
  s.split('\n').map((v) => v.trim()).filter(Boolean)

export function SiteForm({ mode }: Props) {
  const { id = '' } = useParams()
  const [form, setForm] = useState<FormState>(blank)
  const [mintedKey, setMintedKey] = useState<string | null>(null)
  const qc = useQueryClient()
  const navigate = useNavigate()

  const existing = useQuery({
    queryKey: ['site', id],
    queryFn: () => api.getSite(id),
    enabled: mode === 'edit',
  })

  useEffect(() => {
    const s = existing.data
    if (!s) return
    setForm({
      name: s.name,
      url: s.url,
      interval_seconds: String(s.interval_seconds),
      slow_interval_seconds: String(s.slow_interval_seconds),
      enabled: s.enabled,
      httpEnabled: s.checks.http.enabled,
      expectStatus: (s.checks.http.expect_status ?? []).join(', '),
      latencyEnabled: s.checks.latency.enabled,
      latencyMaxMs: String(s.checks.latency.max_ms || 1500),
      contentEnabled: s.checks.content.enabled,
      mustContain: (s.checks.content.must_contain ?? []).join('\n'),
      mustNotContain: (s.checks.content.must_not_contain ?? []).join('\n'),
      sslEnabled: s.checks.ssl.enabled,
      sslWarnDays: String(s.checks.ssl.warn_days || 14),
      dnsEnabled: s.checks.dns.enabled,
      generateKey: false,
    })
  }, [existing.data])

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) =>
    setForm((f) => ({ ...f, [k]: v }))

  const buildBody = () => {
    const expect = form.expectStatus
      .split(',').map((v) => parseInt(v.trim(), 10)).filter((n) => !Number.isNaN(n))
    const body: Record<string, unknown> = {
      name: form.name.trim(),
      url: form.url.trim(),
      interval_seconds: parseInt(form.interval_seconds, 10) || 60,
      slow_interval_seconds: parseInt(form.slow_interval_seconds, 10) || 3600,
      enabled: form.enabled,
      checks: {
        http: { enabled: form.httpEnabled, expect_status: expect.length ? expect : null },
        latency: { enabled: form.latencyEnabled, max_ms: parseInt(form.latencyMaxMs, 10) || 0 },
        content: {
          enabled: form.contentEnabled,
          must_contain: splitList(form.mustContain),
          must_not_contain: splitList(form.mustNotContain),
        },
        ssl: { enabled: form.sslEnabled, warn_days: parseInt(form.sslWarnDays, 10) || 14 },
        dns: { enabled: form.dnsEnabled },
      },
    }
    if (form.generateKey) body.generate_api_key = true
    return body
  }

  const save = useMutation({
    mutationFn: () =>
      mode === 'create' ? api.createSite(buildBody()) : api.updateSite(id, buildBody()),
    onSuccess: (site) => {
      qc.invalidateQueries()
      // A generated key is returned exactly once and only its hash is stored,
      // so it must be shown before navigating away.
      if (site.api_key) {
        setMintedKey(site.api_key)
        return
      }
      navigate(`/site/${site.id}`, { replace: true })
    },
  })

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    save.mutate()
  }

  if (mintedKey) {
    return (
      <div className="card" style={{ maxWidth: 620 }}>
        <h2>Site saved — copy its read key now</h2>
        <div className="notice warn">
          <strong>This key is shown once</strong>
          Only a hash of it is stored, so it cannot be retrieved later. If you lose it,
          you can generate a replacement, which immediately invalidates this one.
        </div>
        <div className="key-reveal">{mintedKey}</div>
        <div className="actions" style={{ marginTop: 14 }}>
          <button className="primary" onClick={() => navigator.clipboard?.writeText(mintedKey)}>
            Copy to clipboard
          </button>
          <button onClick={() => navigate('/', { replace: true })}>I’ve saved it — continue</button>
        </div>
      </div>
    )
  }

  return (
    <form onSubmit={onSubmit} style={{ maxWidth: 720 }}>
      <div className="page-head">
        <h1>{mode === 'create' ? 'Add site' : `Edit ${existing.data?.name ?? ''}`}</h1>
      </div>

      {save.error && (
        <div className="notice err" role="alert">
          <strong>Could not save</strong>
          {save.error.message}
        </div>
      )}

      <div className="card">
        <div className="field">
          <label htmlFor="name">Name</label>
          <input id="name" type="text" required value={form.name}
                 onChange={(e) => set('name', e.target.value)} />
          {mode === 'create' && (
            <span className="hint">
              The id is derived from this and cannot be changed later.
            </span>
          )}
        </div>
        <div className="field">
          <label htmlFor="url">URL to check</label>
          <input id="url" type="url" required placeholder="https://example.com/health"
                 value={form.url} onChange={(e) => set('url', e.target.value)} />
        </div>
        <div className="row fields" style={{ marginBottom: 14 }}>
          <div className="field" style={{ flex: 1, minWidth: 160 }}>
            <label htmlFor="interval">Check every (seconds)</label>
            <input id="interval" type="number" min={5} max={86400} value={form.interval_seconds}
                   onChange={(e) => set('interval_seconds', e.target.value)} />
          </div>
          <div className="field" style={{ flex: 1, minWidth: 160 }}>
            <label htmlFor="slow">Certificate/DNS every (seconds)</label>
            <input id="slow" type="number" min={5} max={86400} value={form.slow_interval_seconds}
                   onChange={(e) => set('slow_interval_seconds', e.target.value)} />
            <span className="hint">Must be at least the check interval.</span>
          </div>
        </div>
        <div className="check">
          <input id="enabled" type="checkbox" checked={form.enabled}
                 onChange={(e) => set('enabled', e.target.checked)} />
          <div className="body">
            <strong>Actively check this site</strong>
            <span className="hint">Uncheck to pause without deleting its history.</span>
          </div>
        </div>
      </div>

      <div className="card">
        <h2>Checks</h2>

        <fieldset>
          <legend>Availability</legend>
          <div className="check">
            <input id="http" type="checkbox" checked={form.httpEnabled}
                   onChange={(e) => set('httpEnabled', e.target.checked)} />
            <div className="body">
              <strong>HTTP status</strong>
              <span className="hint">Fails when the response is not an acceptable status.</span>
            </div>
          </div>
          {form.httpEnabled && (
            <div className="field">
              <label htmlFor="expect">Accepted status codes</label>
              <input id="expect" type="text" placeholder="leave blank to accept any 2xx or 3xx"
                     value={form.expectStatus} onChange={(e) => set('expectStatus', e.target.value)} />
              <span className="hint">Comma separated, e.g. <code className="inline">200, 204</code>.</span>
            </div>
          )}
        </fieldset>

        <fieldset>
          <legend>Speed</legend>
          <div className="check">
            <input id="latency" type="checkbox" checked={form.latencyEnabled}
                   onChange={(e) => set('latencyEnabled', e.target.checked)} />
            <div className="body">
              <strong>Response time limit</strong>
              <span className="hint">A slower response counts as a failed check and opens an incident.</span>
            </div>
          </div>
          {form.latencyEnabled && (
            <div className="field">
              <label htmlFor="maxms">Maximum (ms)</label>
              <input id="maxms" type="number" min={0} value={form.latencyMaxMs}
                     onChange={(e) => set('latencyMaxMs', e.target.value)} />
              <span className="hint">0 records timings without ever failing on them.</span>
            </div>
          )}
        </fieldset>

        <fieldset>
          <legend>Page content</legend>
          <div className="check">
            <input id="content" type="checkbox" checked={form.contentEnabled}
                   onChange={(e) => set('contentEnabled', e.target.checked)} />
            <div className="body">
              <strong>Inspect the response body</strong>
              <span className="hint">Catches a page that returns 200 while showing an error.</span>
            </div>
          </div>
          {form.contentEnabled && (
            <>
              <div className="field">
                <label htmlFor="must">Must contain</label>
                <textarea id="must" value={form.mustContain} placeholder="one phrase per line"
                          onChange={(e) => set('mustContain', e.target.value)} />
              </div>
              <div className="field">
                <label htmlFor="mustnot">Must not contain</label>
                <textarea id="mustnot" value={form.mustNotContain} placeholder={'Exception\nFatal error'}
                          onChange={(e) => set('mustNotContain', e.target.value)} />
              </div>
            </>
          )}
        </fieldset>

        <fieldset>
          <legend>Certificate and DNS</legend>
          <div className="check">
            <input id="ssl" type="checkbox" checked={form.sslEnabled}
                   onChange={(e) => set('sslEnabled', e.target.checked)} />
            <div className="body">
              <strong>TLS certificate</strong>
              <span className="hint">Warns before expiry and fails once expired.</span>
            </div>
          </div>
          {form.sslEnabled && (
            <div className="field">
              <label htmlFor="warn">Warn this many days ahead</label>
              <input id="warn" type="number" min={0} value={form.sslWarnDays}
                     onChange={(e) => set('sslWarnDays', e.target.value)} />
            </div>
          )}
          <div className="check">
            <input id="dns" type="checkbox" checked={form.dnsEnabled}
                   onChange={(e) => set('dnsEnabled', e.target.checked)} />
            <div className="body">
              <strong>DNS resolution</strong>
              <span className="hint">Periodic explicit check that the hostname still resolves.</span>
            </div>
          </div>
        </fieldset>
      </div>

      <div className="card">
        <h2>Read key</h2>
        <p className="subtle" style={{ marginTop: 0 }}>
          A per-site key lets one consuming system read this site’s data and nothing else.
          {existing.data?.has_api_key && ' This site already has one.'}
        </p>
        <div className="check">
          <input id="genkey" type="checkbox" checked={form.generateKey}
                 onChange={(e) => set('generateKey', e.target.checked)} />
          <div className="body">
            <strong>
              {existing.data?.has_api_key ? 'Replace the existing key' : 'Generate a read key'}
            </strong>
            <span className="hint">
              Shown once on save.
              {existing.data?.has_api_key && ' The current key stops working immediately.'}
            </span>
          </div>
        </div>
      </div>

      <div className="actions">
        <button className="primary" type="submit" disabled={save.isPending || !form.name || !form.url}>
          {save.isPending ? 'Saving…' : mode === 'create' ? 'Create site' : 'Save changes'}
        </button>
        <Link className="btn" to={mode === 'edit' ? `/site/${id}` : '/'}>Cancel</Link>
      </div>
    </form>
  )
}
