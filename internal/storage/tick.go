package storage

import (
	"database/sql"
	"strings"
)

// Tick is the outcome of one scheduler tick for a site. The checker builds it;
// RecordTick persists it and updates incident/error/state bookkeeping.
type Tick struct {
	TS           int64
	Up           bool
	StatusCode   int
	ResponseMs   int64
	FailedChecks []string // check types that failed this tick
	Error        string   // primary error message (empty when up)

	// Extra errors to log without necessarily marking the site down (e.g. an
	// SSL cert nearing expiry, a content warning). Each becomes an errors row.
	Errors []CheckError

	// Slow-cadence SSL result. Only applied when SSLChecked is true.
	SSLChecked   bool
	SSLExpiresAt int64 // unix seconds; 0 if unknown
}

// CheckError is a single failure/warning to record in the errors table.
type CheckError struct {
	CheckType string
	Message   string
}

// AddFail records a failed check: it appends the check type to FailedChecks,
// logs an errors row, and sets the primary Error message if not already set.
func (t *Tick) AddFail(checkType, message string) {
	t.FailedChecks = append(t.FailedChecks, checkType)
	t.Errors = append(t.Errors, CheckError{CheckType: checkType, Message: message})
	if t.Error == "" {
		t.Error = message
	}
}

// Transition reports what changed during a RecordTick, so the caller can act on
// it after the transaction commits. Alerting is the reason this exists: the
// down/up edges are already detected here, and recomputing them outside would
// mean a second read racing the write.
type Transition struct {
	// IncidentOpened is set on the tick that takes the site down.
	IncidentOpened bool
	// IncidentResolved is set on the tick that brings it back up.
	IncidentResolved bool
	// IncidentID is the incident opened or resolved by this tick, 0 if neither.
	IncidentID int64
	// ConsecutiveFailures counts failing ticks including this one; 0 when up.
	// Alerting compares it against a rule's confirm_after threshold.
	ConsecutiveFailures int
	// Up is the state this tick recorded.
	Up bool
	// Error is the primary failure message, empty when up.
	Error string
	// SSLExpiresAt carries the certificate expiry when this tick checked it.
	SSLExpiresAt int64
	// SSLChecked reports whether this tick ran the certificate check.
	SSLChecked bool
}

// RecordTick writes the tick row and reconciles incident state in one
// transaction:
//   - a down->up transition resolves the open incident,
//   - an up->down transition opens a new incident,
//   - every failing check logs an errors row.
//
// The returned Transition describes what changed. Act on it only after this
// returns: doing outbound work (sending mail) inside the transaction would hold
// a write lock on the site database for the length of a network round trip.
func (s *SiteStore) RecordTick(t Tick) (Transition, error) {
	tr := Transition{
		Up:           t.Up,
		Error:        t.Error,
		SSLChecked:   t.SSLChecked,
		SSLExpiresAt: t.SSLExpiresAt,
	}

	tx, err := s.db.Begin()
	if err != nil {
		return tr, err
	}
	defer tx.Rollback()

	failed := nullString(strings.Join(t.FailedChecks, ","))
	errMsg := nullString(t.Error)
	if _, err := tx.Exec(`INSERT INTO check_results
		(ts, up, status_code, response_ms, failed_checks, error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.TS, boolToInt(t.Up), t.StatusCode, t.ResponseMs, failed, errMsg); err != nil {
		return tr, err
	}

	// Load current state to detect transitions.
	var (
		curUp    sql.NullInt64
		curIncID sql.NullInt64
		curFails int
	)
	if err := tx.QueryRow(`SELECT current_up, current_incident_id, consecutive_failures
		FROM site_state WHERE id = 1`).Scan(&curUp, &curIncID, &curFails); err != nil {
		return tr, err
	}

	// Reset on any success; otherwise this failure extends the run.
	fails := 0
	if !t.Up {
		fails = curFails + 1
	}
	tr.ConsecutiveFailures = fails

	switch {
	case !t.Up && (!curUp.Valid || curUp.Int64 == 1):
		// Transition into a down state: open an incident.
		res, err := tx.Exec(`INSERT INTO incidents (started_at, cause) VALUES (?, ?)`,
			t.TS, errMsg)
		if err != nil {
			return tr, err
		}
		id, _ := res.LastInsertId()
		curIncID = sql.NullInt64{Int64: id, Valid: true}
		tr.IncidentOpened = true
		tr.IncidentID = id

	case t.Up && curUp.Valid && curUp.Int64 == 0:
		// Recovery: resolve the open incident.
		if curIncID.Valid {
			if _, err := tx.Exec(`UPDATE incidents SET resolved_at = ? WHERE id = ?`,
				t.TS, curIncID.Int64); err != nil {
				return tr, err
			}
			tr.IncidentResolved = true
			tr.IncidentID = curIncID.Int64
		}
		curIncID = sql.NullInt64{} // clear
	}

	// Log every failing check / warning to the errors table.
	for _, ce := range t.Errors {
		if _, err := tx.Exec(`INSERT INTO errors (ts, check_type, message) VALUES (?, ?, ?)`,
			t.TS, ce.CheckType, ce.Message); err != nil {
			return tr, err
		}
	}

	// Update rolled-up state.
	if t.SSLChecked {
		var expires any
		if t.SSLExpiresAt > 0 {
			expires = t.SSLExpiresAt
		}
		if _, err := tx.Exec(`UPDATE site_state SET
			last_check_ts = ?, current_up = ?, current_incident_id = ?,
			ssl_expires_at = ?, ssl_last_checked = ?, consecutive_failures = ?
			WHERE id = 1`,
			t.TS, boolToInt(t.Up), nullInt64(curIncID), expires, t.TS, fails); err != nil {
			return tr, err
		}
	} else {
		if _, err := tx.Exec(`UPDATE site_state SET
			last_check_ts = ?, current_up = ?, current_incident_id = ?,
			consecutive_failures = ? WHERE id = 1`,
			t.TS, boolToInt(t.Up), nullInt64(curIncID), fails); err != nil {
			return tr, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Transition{}, err
	}
	return tr, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}
