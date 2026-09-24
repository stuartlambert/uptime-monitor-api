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
	{
		version: 3,
		// Admin accounts for the web UI. password_hash is a bcrypt digest, which
		// carries its own salt and cost, so no separate columns are needed.
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS admin_users (
				id            INTEGER PRIMARY KEY,
				username      TEXT NOT NULL UNIQUE,
				password_hash TEXT NOT NULL,
				created_at    INTEGER NOT NULL,
				updated_at    INTEGER NOT NULL
			);`,
		},
	},
	{
		version: 4,
		// Sessions are server-side so logout and password changes genuinely
		// revoke, rather than only clearing the browser's copy. The id column
		// holds a SHA-256 of the cookie value, never the value itself: a stolen
		// database then yields no usable cookies.
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS sessions (
				id           TEXT PRIMARY KEY,
				user_id      INTEGER NOT NULL,
				created_at   INTEGER NOT NULL,
				expires_at   INTEGER NOT NULL,
				last_seen_at INTEGER NOT NULL,
				FOREIGN KEY (user_id) REFERENCES admin_users(id) ON DELETE CASCADE
			);`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);`,
		},
	},
	{
		version: 5,
		stmts: []string{
			// Where an alert goes.
			`CREATE TABLE IF NOT EXISTS alert_channels (
				id         INTEGER PRIMARY KEY,
				name       TEXT NOT NULL,
				type       TEXT NOT NULL,
				target     TEXT NOT NULL,
				enabled    INTEGER NOT NULL DEFAULT 1,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			);`,
			// What triggers one. site_id NULL means "every site", so a single
			// rule covers sites added later without further configuration.
			`CREATE TABLE IF NOT EXISTS alert_rules (
				id            INTEGER PRIMARY KEY,
				site_id       TEXT,
				channel_id    INTEGER NOT NULL,
				kind          TEXT NOT NULL,
				enabled       INTEGER NOT NULL DEFAULT 1,
				confirm_after INTEGER NOT NULL DEFAULT 2,
				created_at    INTEGER NOT NULL,
				updated_at    INTEGER NOT NULL,
				FOREIGN KEY (channel_id) REFERENCES alert_channels(id) ON DELETE CASCADE
			);`,
			`CREATE INDEX IF NOT EXISTS idx_alert_rules_site ON alert_rules(site_id);`,
			// The send log, and the dedupe ledger. dedupe_key is UNIQUE and is
			// claimed before sending, so a restart mid-incident cannot re-announce
			// an outage and two workers cannot both send the same alert.
			`CREATE TABLE IF NOT EXISTS alert_deliveries (
				id          INTEGER PRIMARY KEY,
				dedupe_key  TEXT NOT NULL UNIQUE,
				site_id     TEXT NOT NULL,
				kind        TEXT NOT NULL,
				channel_id  INTEGER NOT NULL,
				incident_id INTEGER,
				subject     TEXT NOT NULL,
				status      TEXT NOT NULL,
				attempts    INTEGER NOT NULL DEFAULT 0,
				error       TEXT,
				created_at  INTEGER NOT NULL,
				sent_at     INTEGER
			);`,
			`CREATE INDEX IF NOT EXISTS idx_alert_deliveries_created ON alert_deliveries(created_at DESC);`,
			`CREATE INDEX IF NOT EXISTS idx_alert_deliveries_site ON alert_deliveries(site_id);`,
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
	{
		version: 2,
		// Consecutive failing ticks, reset to 0 on any success. Alerting uses it
		// to hold a down notification until a failure is confirmed, so one blip
		// does not send mail. Kept here rather than derived with a query so
		// RecordTick can maintain it in the transaction it already opens.
		stmts: []string{
			`ALTER TABLE site_state ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0`,
		},
	},
}
