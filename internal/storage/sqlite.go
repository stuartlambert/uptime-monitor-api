package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

// openSQLite opens a SQLite database with pragmas tuned for an append-heavy
// monitoring workload: WAL for concurrent reads during writes, NORMAL sync for
// throughput, and a busy timeout so brief lock contention retries instead of
// failing.
func openSQLite(path string) (*sql.DB, error) {
	// modernc.org/sqlite accepts pragmas via the connection string.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite is single-writer; keep the pool small and predictable.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// SQLite extended result codes for the constraint violations we act on.
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// isUniqueViolation reports whether err is a uniqueness conflict — the signal
// that a row already exists rather than a genuine failure.
//
// It prefers the driver's extended result code and falls back to the message,
// because a code is stable across SQLite versions in a way message text is not.
func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return true
		}
	}
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
