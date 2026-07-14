package storage

import "database/sql"

// Status is the current live state of a site.
type Status struct {
	MonitoringStartedAt int64  `json:"monitoring_started_at"`
	LastCheckTS         *int64 `json:"last_check_ts"`
	Up                  *bool  `json:"up"` // null until the first check runs
	OngoingIncidentID   *int64 `json:"ongoing_incident_id"`
	SSLExpiresAt        *int64 `json:"ssl_expires_at"`
	SSLLastChecked      *int64 `json:"ssl_last_checked"`
}

// Status returns the site's rolled-up state.
func (s *SiteStore) Status() (Status, error) {
	var (
		st      Status
		started sql.NullInt64
		lastTS  sql.NullInt64
		up      sql.NullInt64
		incID   sql.NullInt64
		sslExp  sql.NullInt64
		sslChk  sql.NullInt64
	)
	err := s.db.QueryRow(`SELECT monitoring_started_at, last_check_ts, current_up,
		current_incident_id, ssl_expires_at, ssl_last_checked FROM site_state WHERE id = 1`).
		Scan(&started, &lastTS, &up, &incID, &sslExp, &sslChk)
	if err != nil {
		return Status{}, err
	}
	st.MonitoringStartedAt = started.Int64
	st.LastCheckTS = nullableInt(lastTS)
	if up.Valid {
		b := up.Int64 == 1
		st.Up = &b
	}
	st.OngoingIncidentID = nullableInt(incID)
	st.SSLExpiresAt = nullableInt(sslExp)
	st.SSLLastChecked = nullableInt(sslChk)
	return st, nil
}

// Uptime is the averaged availability over a window.
type Uptime struct {
	Window     string  `json:"window"`
	Checks     int64   `json:"checks"`
	Successful int64   `json:"successful"`
	Failed     int64   `json:"failed"`
	Percent    float64 `json:"uptime_percent"`
}

// Uptime computes availability since `sinceTS` (pass 0 for all-time).
func (s *SiteStore) Uptime(window string, sinceTS int64) (Uptime, error) {
	u := Uptime{Window: window}
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(up), 0)
		FROM check_results WHERE ts >= ?`, sinceTS).Scan(&u.Checks, &u.Successful)
	if err != nil {
		return u, err
	}
	u.Failed = u.Checks - u.Successful
	if u.Checks > 0 {
		u.Percent = float64(u.Successful) / float64(u.Checks) * 100
	}
	return u, nil
}

// Metrics summarises latency and check counts over a window.
type Metrics struct {
	Window     string  `json:"window"`
	Checks     int64   `json:"checks"`
	Successful int64   `json:"successful"`
	Failed     int64   `json:"failed"`
	AvgMs      float64 `json:"avg_ms"`
	P50Ms      int64   `json:"p50_ms"`
	P95Ms      int64   `json:"p95_ms"`
	P99Ms      int64   `json:"p99_ms"`
}

// Metrics computes counts and latency percentiles over successful checks since
// `sinceTS`. Percentiles use an ORDER BY + OFFSET query so we never load the
// whole window into memory.
func (s *SiteStore) Metrics(window string, sinceTS int64) (Metrics, error) {
	m := Metrics{Window: window}
	var avg sql.NullFloat64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(up),0), AVG(response_ms)
		FROM check_results WHERE ts >= ?`, sinceTS).Scan(&m.Checks, &m.Successful, &avg)
	if err != nil {
		return m, err
	}
	m.Failed = m.Checks - m.Successful
	m.AvgMs = avg.Float64

	for _, p := range []struct {
		q   float64
		dst *int64
	}{{0.50, &m.P50Ms}, {0.95, &m.P95Ms}, {0.99, &m.P99Ms}} {
		v, err := s.percentile(sinceTS, p.q)
		if err != nil {
			return m, err
		}
		*p.dst = v
	}
	return m, nil
}

// percentile returns the response_ms at quantile q over successful checks.
func (s *SiteStore) percentile(sinceTS int64, q float64) (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_results
		WHERE ts >= ? AND up = 1 AND response_ms IS NOT NULL`, sinceTS).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	offset := int64(q * float64(n))
	if offset >= n {
		offset = n - 1
	}
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT response_ms FROM check_results
		WHERE ts >= ? AND up = 1 AND response_ms IS NOT NULL
		ORDER BY response_ms LIMIT 1 OFFSET ?`, sinceTS, offset).Scan(&v)
	if err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// ErrorRow is a stored error/warning.
type ErrorRow struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"`
	CheckType string `json:"check_type"`
	Message   string `json:"message"`
}

// Errors returns stored errors since `sinceTS` (0 = all), newest first.
func (s *SiteStore) Errors(sinceTS int64, limit int) ([]ErrorRow, error) {
	rows, err := s.db.Query(`SELECT id, ts, COALESCE(check_type,''), COALESCE(message,'')
		FROM errors WHERE ts >= ? ORDER BY id DESC LIMIT ?`, sinceTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ErrorRow{}
	for rows.Next() {
		var e ErrorRow
		if err := rows.Scan(&e.ID, &e.TS, &e.CheckType, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Incident is a contiguous downtime period.
type Incident struct {
	ID         int64  `json:"id"`
	StartedAt  int64  `json:"started_at"`
	ResolvedAt *int64 `json:"resolved_at"` // null while ongoing
	Cause      string `json:"cause"`
	DurationS  *int64 `json:"duration_seconds"` // null while ongoing
}

// Incidents returns incidents newest first.
func (s *SiteStore) Incidents(limit int) ([]Incident, error) {
	rows, err := s.db.Query(`SELECT id, started_at, resolved_at, COALESCE(cause,'')
		FROM incidents ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		var (
			in       Incident
			resolved sql.NullInt64
		)
		if err := rows.Scan(&in.ID, &in.StartedAt, &resolved, &in.Cause); err != nil {
			return nil, err
		}
		if resolved.Valid {
			r := resolved.Int64
			in.ResolvedAt = &r
			d := r - in.StartedAt
			in.DurationS = &d
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ResultRow is a raw check_results row.
type ResultRow struct {
	ID           int64  `json:"id"`
	TS           int64  `json:"ts"`
	Up           bool   `json:"up"`
	StatusCode   int    `json:"status_code"`
	ResponseMs   int64  `json:"response_ms"`
	FailedChecks string `json:"failed_checks,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Results returns raw check rows since `sinceTS` (0 = all), newest first.
func (s *SiteStore) Results(sinceTS int64, limit int) ([]ResultRow, error) {
	rows, err := s.db.Query(`SELECT id, ts, up, COALESCE(status_code,0),
		COALESCE(response_ms,0), COALESCE(failed_checks,''), COALESCE(error,'')
		FROM check_results WHERE ts >= ? ORDER BY id DESC LIMIT ?`, sinceTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResultRow{}
	for rows.Next() {
		var (
			r  ResultRow
			up int
		)
		if err := rows.Scan(&r.ID, &r.TS, &up, &r.StatusCode, &r.ResponseMs,
			&r.FailedChecks, &r.Error); err != nil {
			return nil, err
		}
		r.Up = up == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullableInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
