package storage

import (
	"database/sql"
	"fmt"
)

// migration is one ordered schema change. Version numbers must be dense and
// increasing starting at 1; each migration runs exactly once per database and
// advances PRAGMA user_version.
type migration struct {
	version int
	stmts   []string
}

// applyMigrations brings db up to the latest version in migs. It uses SQLite's
// user_version header field to track which migrations have been applied. Each
// migration (and its user_version bump) runs in a single transaction, so a
// failure leaves the database at its previous version rather than half-migrated.
//
// Migration v1 is the baseline and uses CREATE TABLE IF NOT EXISTS so it is safe
// to apply to databases created before migrations existed (they report
// user_version 0 but already have the tables).
func applyMigrations(db *sql.DB, migs []migration) error {
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration v%d: %w", m.version, err)
			}
		}
		// PRAGMA user_version cannot be parameterised; the value is an integer
		// from our own migration list, never user input.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d set version: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration v%d commit: %w", m.version, err)
		}
	}
	return nil
}

// registryMigrations is the ordered schema history for registry.db.
var registryMigrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS sites (
				id                    TEXT PRIMARY KEY,
				name                  TEXT NOT NULL,
				url                   TEXT NOT NULL,
				interval_seconds      INTEGER NOT NULL,
				slow_interval_seconds INTEGER NOT NULL,
				enabled               INTEGER NOT NULL,
				checks_json           TEXT NOT NULL,
				created_at            INTEGER NOT NULL,
				updated_at            INTEGER NOT NULL
			);`,
		},
	},
	{
		version: 2,
		// Per-site read key (SHA-256 hex), NULL when the site has no dedicated key.
		stmts: []string{
			`ALTER TABLE sites ADD COLUMN api_key_hash TEXT`,
		},
	},
}

// siteMigrations is the ordered schema history for each per-site database.
var siteMigrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS check_results (
				id            INTEGER PRIMARY KEY,
				ts            INTEGER NOT NULL,
				up            INTEGER NOT NULL,
				status_code   INTEGER,
				response_ms   INTEGER,
				failed_checks TEXT,
				error         TEXT
			);`,
			`CREATE TABLE IF NOT EXISTS incidents (
				id          INTEGER PRIMARY KEY,
				started_at  INTEGER NOT NULL,
				resolved_at INTEGER,
				cause       TEXT
			);`,
			`CREATE TABLE IF NOT EXISTS errors (
				id         INTEGER PRIMARY KEY,
				ts         INTEGER NOT NULL,
				check_type TEXT,
				message    TEXT
			);`,
			`CREATE TABLE IF NOT EXISTS site_state (
				id                    INTEGER PRIMARY KEY CHECK (id = 1),
				monitoring_started_at INTEGER,
				last_check_ts         INTEGER,
				current_up            INTEGER,
				current_incident_id   INTEGER,
				ssl_expires_at        INTEGER,
				ssl_last_checked      INTEGER
			);`,
		},
	},
}
