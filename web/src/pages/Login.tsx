import { useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api } from '../api/client'

export function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const qc = useQueryClient()
  const navigate = useNavigate()

  const login = useMutation({
    mutationFn: () => api.login(username, password),
    onSuccess: async () => {
      await qc.invalidateQueries()
      navigate('/', { replace: true })
    },
  })

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    login.mutate()
  }

  // The server deliberately returns one message for both a bad username and a
  // bad password; repeating it verbatim keeps the UI from implying otherwise.
  const message = login.error?.message

  return (
    <div className="login-wrap">
      <form className="card login-card" onSubmit={onSubmit}>
        <div className="brand"><span className="beacon" />Uptime Monitor</div>
        <p className="tagline">Sign in to manage monitored sites and alerts.</p>

        {message && (
          <div className="notice err" role="alert">
            {message}
          </div>
        )}

        <div className="field">
          <label htmlFor="username">Username</label>
          <input id="username" type="text" autoComplete="username" autoFocus required
                 value={username} onChange={(e) => setUsername(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="password">Password</label>
          <input id="password" type="password" autoComplete="current-password" required
                 value={password} onChange={(e) => setPassword(e.target.value)} />
        </div>

        <button className="primary" type="submit" style={{ width: '100%', justifyContent: 'center' }}
                disabled={login.isPending || !username || !password}>
          {login.isPending ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </div>
  )
}
