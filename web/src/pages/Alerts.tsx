import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import type { AlertKind } from '../api/types'
import { fmtTime } from '../format'

const KIND_LABEL: Record<AlertKind, string> = {
  site_down: 'Site goes down',
  site_recovered: 'Site recovers',
  ssl_expiring: 'Certificate expiring',
}

export function Alerts() {
  const qc = useQueryClient()
  const channels = useQuery({ queryKey: ['channels'], queryFn: api.channels })
  const rules = useQuery({ queryKey: ['rules'], queryFn: api.rules })
  const sites = useQuery({ queryKey: ['sites'], queryFn: api.listSites })
  const deliveries = useQuery({
    queryKey: ['deliveries'], queryFn: () => api.deliveries(50), refetchInterval: 60_000,
  })

  const [name, setName] = useState('')
  const [target, setTarget] = useState('')
  const [testResult, setTestResult] = useState<{ ok: boolean; text: string } | null>(null)

  const addChannel = useMutation({
    mutationFn: () => api.createChannel({ name: name.trim(), type: 'email', target: target.trim() }),
    onSuccess: () => {
      setName(''); setTarget('')
      qc.invalidateQueries({ queryKey: ['channels'] })
    },
  })
  const delChannel = useMutation({
    mutationFn: (id: number) => api.deleteChannel(id),
    onSuccess: () => qc.invalidateQueries(),
  })
  const toggleChannel = useMutation({
    mutationFn: ({ id, body }: { id: number; body: unknown }) => api.updateChannel(id, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['channels'] }),
  })
  const testChannel = useMutation({
    mutationFn: (id: number) => api.testChannel(id),
    onSuccess: (r) => setTestResult({ ok: true, text: `Test message sent to ${r.target}.` }),
    onError: (e) => setTestResult({ ok: false, text: e.message }),
  })

  const [ruleChannel, setRuleChannel] = useState('')
  const [ruleKind, setRuleKind] = useState<AlertKind>('site_down')
  const [ruleSite, setRuleSite] = useState('')
  const [confirmAfter, setConfirmAfter] = useState('2')

  const addRule = useMutation({
    mutationFn: () => api.createRule({
      channel_id: Number(ruleChannel),
      kind: ruleKind,
      site_id: ruleSite || null,
      confirm_after: parseInt(confirmAfter, 10) || 2,
    }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['rules'] }),
  })
  const delRule = useMutation({
    mutationFn: (id: number) => api.deleteRule(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['rules'] }),
  })

  const chans = channels.data ?? []
  const ruleList = rules.data ?? []

  // A channel that can be told a site went down but never that it recovered is
  // a real configuration trap, so surface it rather than leaving it silent.
  const lopsided = chans.filter((c) => {
    const kinds = ruleList.filter((r) => r.channel_id === c.id && r.enabled).map((r) => r.kind)
    return kinds.includes('site_down') && !kinds.includes('site_recovered')
  })

  return (
    <>
      <div className="page-head"><h1>Alerts</h1></div>

      <div className="card">
        <div className="card-head"><h2>Where alerts go</h2></div>

        {testResult && (
          <div className={`notice ${testResult.ok ? 'ok' : 'err'}`} role="status">
            {testResult.text}
          </div>
        )}
        {addChannel.error && <div className="notice err">{addChannel.error.message}</div>}

        {chans.length === 0 ? (
          <p className="subtle">No destinations yet. Nothing will be sent until you add one.</p>
        ) : (
          <div className="table-wrap" style={{ marginBottom: 16 }}>
            <table>
              <thead><tr><th>Name</th><th>Email</th><th>State</th><th></th></tr></thead>
              <tbody>
                {chans.map((c) => (
                  <tr key={c.id}>
                    <td style={{ fontWeight: 600 }}>{c.name}</td>
                    <td className="mono" style={{ fontSize: 13 }}>{c.target}</td>
                    <td>
                      {c.enabled
                        ? <span className="pill up"><span className="dot" />Active</span>
                        : <span className="pill unknown"><span className="dot" />Muted</span>}
                    </td>
                    <td>
                      <div className="actions">
                        <button className="small" disabled={testChannel.isPending}
                                onClick={() => { setTestResult(null); testChannel.mutate(c.id) }}>
                          Send test
                        </button>
                        <button className="small ghost"
                                onClick={() => toggleChannel.mutate({
                                  id: c.id,
                                  body: { name: c.name, type: c.type, target: c.target, enabled: !c.enabled },
                                })}>
                          {c.enabled ? 'Mute' : 'Unmute'}
                        </button>
                        <button className="small danger" onClick={() => delChannel.mutate(c.id)}>
                          Delete
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <h3>Add a destination</h3>
        <div className="row" style={{ alignItems: 'flex-end' }}>
          <div className="field" style={{ flex: 1, minWidth: 160, marginBottom: 0 }}>
            <label htmlFor="cname">Label</label>
            <input id="cname" type="text" value={name} placeholder="Ops"
                   onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="field" style={{ flex: 2, minWidth: 220, marginBottom: 0 }}>
            <label htmlFor="ctarget">Email address</label>
            <input id="ctarget" type="text" value={target} placeholder="you@example.com"
                   onChange={(e) => setTarget(e.target.value)} />
          </div>
          <button className="primary" disabled={!name || !target || addChannel.isPending}
                  onClick={() => addChannel.mutate()}>
            Add
          </button>
        </div>
      </div>

      <div className="card">
        <div className="card-head"><h2>When to send</h2></div>

        {lopsided.length > 0 && (
          <div className="notice warn">
            <strong>Recovery notices are not configured for {lopsided.map((c) => c.name).join(', ')}</strong>
            A destination is only told a site recovered if it was also told the site went down.
            Add a “Site recovers” rule for the same destination.
          </div>
        )}
        {addRule.error && <div className="notice err">{addRule.error.message}</div>}

        {ruleList.length === 0 ? (
          <p className="subtle">No rules yet, so no alerts will be sent.</p>
        ) : (
          <div className="table-wrap" style={{ marginBottom: 16 }}>
            <table>
              <thead><tr><th>Trigger</th><th>Site</th><th>Destination</th><th>Confirm after</th><th></th></tr></thead>
              <tbody>
                {ruleList.map((r) => (
                  <tr key={r.id}>
                    <td style={{ fontWeight: 600 }}>{KIND_LABEL[r.kind] ?? r.kind}</td>
                    <td>{r.site_id ? <code className="inline">{r.site_id}</code> : <span className="subtle">All sites</span>}</td>
                    <td>{chans.find((c) => c.id === r.channel_id)?.name ?? `#${r.channel_id}`}</td>
                    <td className="num">{r.kind === 'site_down' ? `${r.confirm_after} checks` : '—'}</td>
                    <td>
                      <button className="small danger" onClick={() => delRule.mutate(r.id)}>Delete</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <h3>Add a rule</h3>
        {chans.length === 0 ? (
          <p className="subtle">Add a destination first.</p>
        ) : (
          <div className="row" style={{ alignItems: 'flex-end' }}>
            <div className="field" style={{ flex: 1, minWidth: 170, marginBottom: 0 }}>
              <label htmlFor="rkind">Trigger</label>
              <select id="rkind" value={ruleKind} onChange={(e) => setRuleKind(e.target.value as AlertKind)}>
                {(Object.keys(KIND_LABEL) as AlertKind[]).map((k) => (
                  <option key={k} value={k}>{KIND_LABEL[k]}</option>
                ))}
              </select>
            </div>
            <div className="field" style={{ flex: 1, minWidth: 150, marginBottom: 0 }}>
              <label htmlFor="rsite">Site</label>
              <select id="rsite" value={ruleSite} onChange={(e) => setRuleSite(e.target.value)}>
                <option value="">All sites</option>
                {(sites.data ?? []).map((s) => (
                  <option key={s.id} value={s.id}>{s.name}</option>
                ))}
              </select>
            </div>
            <div className="field" style={{ flex: 1, minWidth: 150, marginBottom: 0 }}>
              <label htmlFor="rchan">Destination</label>
              <select id="rchan" value={ruleChannel} onChange={(e) => setRuleChannel(e.target.value)}>
                <option value="">Choose…</option>
                {chans.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
              </select>
            </div>
            {ruleKind === 'site_down' && (
              <div className="field" style={{ width: 130, marginBottom: 0 }}>
                <label htmlFor="rconfirm">Confirm after</label>
                <input id="rconfirm" type="number" min={1} max={100} value={confirmAfter}
                       onChange={(e) => setConfirmAfter(e.target.value)} />
              </div>
            )}
            <button className="primary" disabled={!ruleChannel || addRule.isPending}
                    onClick={() => addRule.mutate()}>
              Add rule
            </button>
          </div>
        )}
        {ruleKind === 'site_down' && (
          <p className="subtle" style={{ fontSize: 12, marginTop: 10, marginBottom: 0 }}>
            Waiting for consecutive failures means one blip sends nothing, at the cost of
            delaying a genuine alert by that many check intervals.
          </p>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Sent alerts</h2>
          <span className="spacer" />
          <span className="subtle" style={{ fontSize: 12 }}>Most recent 50</span>
        </div>
        {(deliveries.data ?? []).length === 0 ? (
          <p className="subtle" style={{ margin: 0 }}>Nothing sent yet.</p>
        ) : (
          <div className="table-wrap">
            <table>
              <thead><tr><th>When</th><th>Subject</th><th>Site</th><th>Result</th></tr></thead>
              <tbody>
                {deliveries.data!.map((d) => (
                  <tr key={d.id}>
                    <td className="mono" style={{ fontSize: 12 }}>{fmtTime(d.sent_at ?? d.created_at)}</td>
                    <td>{d.subject}</td>
                    <td><code className="inline">{d.site_id}</code></td>
                    <td>
                      {d.status === 'sent' && <span className="pill up"><span className="dot" />Sent</span>}
                      {d.status === 'pending' && <span className="pill unknown"><span className="dot" />Pending</span>}
                      {d.status === 'failed' && (
                        <>
                          <span className="pill down"><span className="dot" />Failed</span>
                          <div className="subtle" style={{ fontSize: 11, marginTop: 3 }}>
                            {d.error} · {d.attempts} attempt{d.attempts === 1 ? '' : 's'}
                          </div>
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}
