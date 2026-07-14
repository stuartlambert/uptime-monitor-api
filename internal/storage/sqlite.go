package storage

import (
	"database/sql"
	"fmt"
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
