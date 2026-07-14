package storage

import (
	"database/sql"
	"fmt"
)

// SiteStore is the per-site database: one SQLite file holding that site's check
// results, incidents, errors, and rolled-up state.
//
// Lean schema: one row per tick in check_results. The integer primary key is
// assigned in insertion (time) order, so rowid already orders rows by time and
// we deliberately omit a ts index to keep the file small (see docs/STORAGE.md).
type SiteStore struct {
	db *sql.DB
}

func openSiteStore(path string) (*SiteStore, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	if err := applyMigrations(db, siteMigrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("site migrations: %w", err)
	}
	// Ensure the singleton state row exists and stamp the monitoring start time.
	if _, err := db.Exec(`INSERT INTO site_state (id, monitoring_started_at)
		VALUES (1, ?) ON CONFLICT(id) DO NOTHING`, Now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("init site_state: %w", err)
	}
	return &SiteStore{db: db}, nil
}

// Close closes the underlying database.
func (s *SiteStore) Close() error { return s.db.Close() }
