import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api } from '../api/client'

export function Settings() {
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [repeat, setRepeat] = useState('')
  const [localError, setLocalError] = useState('')
  const qc = useQueryClient()
  const navigate = useNavigate()

  const change = useMutation({
    mutationFn: () => api.changePassword(current, next),
    onSuccess: () => {
      // The server revokes every session on a password change, including this
      // one, so there is nothing to stay signed in to.
      qc.clear()
      navigate('/login', { replace: true })
    },
  })

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    setLocalError('')
    if (next !== repeat) {
      setLocalError('The new passwords do not match.')
      return
    }
    if (next.length < 12) {
      setLocalError('Use at least 12 characters.')
      return
    }
    change.mutate()
  }

  const viaKey = me.data?.auth === 'api_key'

  return (
    <>
      <div className="page-head"><h1>Settings</h1></div>

      <div className="card" style={{ maxWidth: 520 }}>
        <h2>Change password</h2>
        <p className="subtle" style={{ marginTop: 0 }}>
          Signed in as <strong>{me.data?.username ?? '—'}</strong>. Changing the password signs
          out every browser, including this one.
        </p>

        {viaKey && (
          <div className="notice warn">
            <strong>Signed in with an API key</strong>
            Passwords can only be changed from a logged-in session.
          </div>
        )}
        {(localError || change.error) && (
          <div className="notice err" role="alert">{localError || change.error?.message}</div>
        )}

        <form onSubmit={onSubmit}>
          <div className="field">
            <label htmlFor="cur">Current password</label>
            <input id="cur" type="password" autoComplete="current-password" required
                   value={current} onChange={(e) => setCurrent(e.target.value)} disabled={viaKey} />
          </div>
          <div className="field">
            <label htmlFor="new">New password</label>
            <input id="new" type="password" autoComplete="new-password" required
                   value={next} onChange={(e) => setNext(e.target.value)} disabled={viaKey} />
            <span className="hint">At least 12 characters. Length matters more than symbols.</span>
          </div>
          <div className="field">
            <label htmlFor="rep">Repeat new password</label>
            <input id="rep" type="password" autoComplete="new-password" required
                   value={repeat} onChange={(e) => setRepeat(e.target.value)} disabled={viaKey} />
          </div>
          <button className="primary" type="submit"
                  disabled={viaKey || change.isPending || !current || !next}>
            {change.isPending ? 'Updating…' : 'Change password'}
          </button>
        </form>
      </div>

      <div className="card" style={{ maxWidth: 520 }}>
        <h2>Locked out?</h2>
        <p className="subtle" style={{ marginTop: 0, marginBottom: 8 }}>
          Five failed attempts for a username or address blocks sign-in for 15 minutes.
          The counter is held in memory, so restarting the service clears it. To reset a
          password from the server:
        </p>
        <div className="key-reveal">
          monitor -data /var/lib/uptime-monitor -set-password {me.data?.username ?? 'username'}
        </div>
      </div>
    </>
  )
}
