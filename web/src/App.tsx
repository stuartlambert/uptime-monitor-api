import { NavLink, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, UnauthorizedError } from './api/client'
import { Login } from './pages/Login'
import { Dashboard } from './pages/Dashboard'
import { SiteDetail } from './pages/SiteDetail'
import { SiteForm } from './pages/SiteForm'
import { Alerts } from './pages/Alerts'
import { Settings } from './pages/Settings'

export default function App() {
  const location = useLocation()

  // A single source of truth for "am I signed in": the API itself. No token is
  // kept in JS — the session lives in an HttpOnly cookie the page cannot read.
  const me = useQuery({
    queryKey: ['me'],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  })

  if (me.isLoading) {
    return (
      <div className="login-wrap">
        <div className="skeleton" style={{ width: 220, height: 20 }} />
      </div>
    )
  }

  const signedOut = me.error instanceof UnauthorizedError

  if (signedOut) {
    if (location.pathname !== '/login') {
      return <Navigate to="/login" replace state={{ from: location.pathname }} />
    }
    return (
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    )
  }

  if (me.error) {
    return (
      <div className="login-wrap">
        <div className="card login-card">
          <div className="notice err">
            <strong>Cannot reach the monitor</strong>
            {me.error.message}
          </div>
          <button onClick={() => me.refetch()}>Retry</button>
        </div>
      </div>
    )
  }

  return (
    <div className="shell">
      <TopBar username={me.data?.username ?? null} />
      <main className="main">
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/login" element={<Navigate to="/" replace />} />
          <Route path="/site/new" element={<SiteForm mode="create" />} />
          <Route path="/site/:id/edit" element={<SiteForm mode="edit" />} />
          <Route path="/site/:id" element={<SiteDetail />} />
          <Route path="/alerts" element={<Alerts />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="*" element={<NotFound />} />
        </Routes>
      </main>
    </div>
  )
}

function TopBar({ username }: { username: string | null }) {
  const qc = useQueryClient()
  const navigate = useNavigate()

  // Polled so the beacon reflects the estate, not just the page you are on.
  const overview = useQuery({
    queryKey: ['overview', '24h'],
    queryFn: () => api.overview('24h'),
    refetchInterval: 30_000,
  })
  const anyDown = (overview.data ?? []).some((r) => r.site.enabled && r.up === false)

  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      qc.clear()
      navigate('/login', { replace: true })
    },
  })

  return (
    <header className="topbar">
      <NavLink to="/" className="brand">
        <span className={`beacon${anyDown ? ' bad' : ''}`} />
        Uptime Monitor
      </NavLink>
      <nav className="nav">
        <NavLink to="/" end className={({ isActive }) => (isActive ? 'active' : '')}>Dashboard</NavLink>
        <NavLink to="/alerts" className={({ isActive }) => (isActive ? 'active' : '')}>Alerts</NavLink>
        <NavLink to="/settings" className={({ isActive }) => (isActive ? 'active' : '')}>Settings</NavLink>
      </nav>
      {username && <span className="who">{username}</span>}
      <button className="ghost small" onClick={() => logout.mutate()} disabled={logout.isPending}>
        Sign out
      </button>
    </header>
  )
}

function NotFound() {
  return (
    <div className="empty">
      <h3>Page not found</h3>
      <p>That address doesn’t match anything here.</p>
      <NavLink className="btn" to="/">Back to dashboard</NavLink>
    </div>
  )
}
