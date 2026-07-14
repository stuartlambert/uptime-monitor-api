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

// RecordTick writes the tick row and reconciles incident state in one
// transaction:
//   - a down->up transition resolves the open incident,
//   - an up->down transition opens a new incident,
//   - every failing check logs an errors row.
func (s *SiteStore) RecordTick(t Tick) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	failed := nullString(strings.Join(t.FailedChecks, ","))
	errMsg := nullString(t.Error)
	if _, err := tx.Exec(`INSERT INTO check_results
		(ts, up, status_code, response_ms, failed_checks, error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.TS, boolToInt(t.Up), t.StatusCode, t.ResponseMs, failed, errMsg); err != nil {
		return err
	}

	// Load current state to detect transitions.
	var (
		curUp    sql.NullInt64
		curIncID sql.NullInt64
	)
	if err := tx.QueryRow(`SELECT current_up, current_incident_id FROM site_state WHERE id = 1`).
		Scan(&curUp, &curIncID); err != nil {
		return err
	}

	switch {
	case !t.Up && (!curUp.Valid || curUp.Int64 == 1):
		// Transition into a down state: open an incident.
		res, err := tx.Exec(`INSERT INTO incidents (started_at, cause) VALUES (?, ?)`,
			t.TS, errMsg)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		curIncID = sql.NullInt64{Int64: id, Valid: true}

	case t.Up && curUp.Valid && curUp.Int64 == 0:
		// Recovery: resolve the open incident.
		if curIncID.Valid {
			if _, err := tx.Exec(`UPDATE incidents SET resolved_at = ? WHERE id = ?`,
				t.TS, curIncID.Int64); err != nil {
				return err
			}
		}
		curIncID = sql.NullInt64{} // clear
	}

	// Log every failing check / warning to the errors table.
	for _, ce := range t.Errors {
		if _, err := tx.Exec(`INSERT INTO errors (ts, check_type, message) VALUES (?, ?, ?)`,
			t.TS, ce.CheckType, ce.Message); err != nil {
			return err
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
			ssl_expires_at = ?, ssl_last_checked = ? WHERE id = 1`,
			t.TS, boolToInt(t.Up), nullInt64(curIncID), expires, t.TS); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE site_state SET
			last_check_ts = ?, current_up = ?, current_incident_id = ? WHERE id = 1`,
			t.TS, boolToInt(t.Up), nullInt64(curIncID)); err != nil {
			return err
		}
	}

	return tx.Commit()
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
