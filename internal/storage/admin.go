package storage

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrUserNotFound is returned when no admin account matches.
var ErrUserNotFound = errors.New("admin user not found")

// AdminUser is a login account for the web UI. PasswordHash is a bcrypt digest;
// this package never sees a plaintext password, only what internal/auth hands it.
type AdminUser struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    int64
	UpdatedAt    int64
}

// normaliseUsername lower-cases and trims so "Stuart" and "stuart " are the same
// account. Applied on every read and write, so the UNIQUE index is meaningful.
func normaliseUsername(u string) string {
	return strings.ToLower(strings.TrimSpace(u))
}

// AdminUserCount reports how many admin accounts exist. Callers use it to decide
// whether the first-boot seed should run and whether auth can be enforced.
func (r *Registry) AdminUserCount() (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&n)
	return n, err
}

// AdminUserByName looks up an account. It returns ErrUserNotFound when there is
// no match, which callers must treat identically to a bad password.
func (r *Registry) AdminUserByName(username string) (AdminUser, error) {
	var u AdminUser
	err := r.db.QueryRow(`SELECT id, username, password_hash, created_at, updated_at
		FROM admin_users WHERE username = ?`, normaliseUsername(username)).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUserNotFound
	}
	return u, err
}

// AdminUserByID looks up an account by primary key (the session path).
func (r *Registry) AdminUserByID(id int64) (AdminUser, error) {
	var u AdminUser
	err := r.db.QueryRow(`SELECT id, username, password_hash, created_at, updated_at
		FROM admin_users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUserNotFound
	}
	return u, err
}

// CreateAdminUser inserts an account and returns its id.
func (r *Registry) CreateAdminUser(username, passwordHash string) (int64, error) {
	now := time.Now().Unix()
	res, err := r.db.Exec(`INSERT INTO admin_users
		(username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		normaliseUsername(username), passwordHash, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetAdminPassword replaces an account's password hash and revokes every session
// that account holds, in one transaction. Revocation is the point: a password
// change that leaves old sessions valid does not lock out whoever prompted it.
func (r *Registry) SetAdminPassword(userID int64, passwordHash string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE admin_users SET password_hash = ?, updated_at = ?
		WHERE id = ?`, passwordHash, time.Now().Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
