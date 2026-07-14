// Package storage handles all persistence: the registry database (site configs)
// and one SQLite database file per monitored site (check results, incidents,
// errors, and rolled-up state).
package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// ErrNotFound is returned when a site id does not exist in the registry.
var ErrNotFound = errors.New("site not found")

// Registry stores the list of monitored sites and their configuration.
type Registry struct {
	db *sql.DB
}

// OpenRegistry opens (or creates) the registry database at path.
func OpenRegistry(path string) (*Registry, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	if err := applyMigrations(db, registryMigrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("registry migrations: %w", err)
	}
	return &Registry{db: db}, nil
}

// Close closes the registry database.
func (r *Registry) Close() error { return r.db.Close() }

// List returns all site configs ordered by id.
func (r *Registry) List() ([]config.SiteConfig, error) {
	rows, err := r.db.Query(`SELECT id, name, url, interval_seconds, slow_interval_seconds,
		enabled, checks_json, created_at, updated_at, COALESCE(api_key_hash, '')
		FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []config.SiteConfig
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns a single site config, or ErrNotFound.
func (r *Registry) Get(id string) (config.SiteConfig, error) {
	row := r.db.QueryRow(`SELECT id, name, url, interval_seconds, slow_interval_seconds,
		enabled, checks_json, created_at, updated_at, COALESCE(api_key_hash, '')
		FROM sites WHERE id = ?`, id)
	s, err := scanSite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return config.SiteConfig{}, ErrNotFound
	}
	return s, err
}

// Exists reports whether a site id is already registered.
func (r *Registry) Exists(id string) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// Create inserts a new site. The caller is responsible for defaults/validation.
func (r *Registry) Create(s config.SiteConfig) error {
	checksJSON, err := json.Marshal(s.Checks)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(`INSERT INTO sites
		(id, name, url, interval_seconds, slow_interval_seconds, enabled, checks_json, created_at, updated_at, api_key_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.URL, s.IntervalSeconds, s.SlowIntervalSeconds,
		boolToInt(s.Enabled), string(checksJSON), s.CreatedAt, s.UpdatedAt, nullString(s.APIKeyHash))
	return err
}

// Update replaces an existing site's mutable fields.
func (r *Registry) Update(s config.SiteConfig) error {
	checksJSON, err := json.Marshal(s.Checks)
	if err != nil {
		return err
	}
	res, err := r.db.Exec(`UPDATE sites SET
		name = ?, url = ?, interval_seconds = ?, slow_interval_seconds = ?,
		enabled = ?, checks_json = ?, updated_at = ?, api_key_hash = ?
		WHERE id = ?`,
		s.Name, s.URL, s.IntervalSeconds, s.SlowIntervalSeconds,
		boolToInt(s.Enabled), string(checksJSON), s.UpdatedAt, nullString(s.APIKeyHash), s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a site from the registry. It does NOT delete the per-site data
// file; the caller (storage.Manager) handles that so the two stay in sync.
func (r *Registry) Delete(id string) error {
	res, err := r.db.Exec(`DELETE FROM sites WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSite(s rowScanner) (config.SiteConfig, error) {
	var (
		c          config.SiteConfig
		enabled    int
		checksJSON string
	)
	if err := s.Scan(&c.ID, &c.Name, &c.URL, &c.IntervalSeconds, &c.SlowIntervalSeconds,
		&enabled, &checksJSON, &c.CreatedAt, &c.UpdatedAt, &c.APIKeyHash); err != nil {
		return config.SiteConfig{}, err
	}
	c.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(checksJSON), &c.Checks); err != nil {
		return config.SiteConfig{}, fmt.Errorf("decode checks for %s: %w", c.ID, err)
	}
	return c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Now returns the current unix time; centralised so tests could override it.
func Now() int64 { return time.Now().Unix() }
