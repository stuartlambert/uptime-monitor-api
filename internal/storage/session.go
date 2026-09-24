package storage

import (
	"database/sql"
	"errors"
	"time"
)

// ErrSessionNotFound is returned when a session id is unknown or expired.
var ErrSessionNotFound = errors.New("session not found")

// Session is one logged-in browser. ID is the SHA-256 of the cookie value the
// browser holds, never the value itself — see migration v4.
type Session struct {
	ID         string
	UserID     int64
	CreatedAt  int64
	ExpiresAt  int64
	LastSeenAt int64
}

// CreateSession stores a session that expires at expiresAt.
func (r *Registry) CreateSession(id string, userID, expiresAt int64) error {
	now := time.Now().Unix()
	_, err := r.db.Exec(`INSERT INTO sessions
		(id, user_id, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		id, userID, now, expiresAt, now)
	return err
}

// LookupSession returns a live session. Expired rows are reported as not found
// and deleted, so expiry needs no separate sweep to be correct — PurgeExpiredSessions
// only stops the table growing.
func (r *Registry) LookupSession(id string) (Session, error) {
	var s Session
	err := r.db.QueryRow(`SELECT id, user_id, created_at, expires_at, last_seen_at
		FROM sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrSessionNotFound
	}
	if err != nil {
		return s, err
	}
	if time.Now().Unix() >= s.ExpiresAt {
		r.DeleteSession(id) // best effort; the caller is being rejected either way
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

// TouchSession slides the expiry window forward on use, so an active admin is
// not logged out mid-session.
func (r *Registry) TouchSession(id string, expiresAt int64) error {
	_, err := r.db.Exec(`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		time.Now().Unix(), expiresAt, id)
	return err
}

// DeleteSession revokes one session (logout).
func (r *Registry) DeleteSession(id string) error {
	_, err := r.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// PurgeExpiredSessions removes rows whose expiry has passed, returning how many
// went. Housekeeping only: LookupSession already refuses expired sessions.
func (r *Registry) PurgeExpiredSessions() (int64, error) {
	res, err := r.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
