package storage

import (
	"path/filepath"
	"testing"
)

func userVersion(t *testing.T, s *SiteStore) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrationsAdvanceVersion(t *testing.T) {
	s := newStore(t) // openSiteStore applies siteMigrations
	if got, want := userVersion(t, s), len(siteMigrations); got != want {
		t.Fatalf("user_version = %d, want %d", got, want)
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")

	s1, err := openSiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	v1 := userVersion(t, s1)
	s1.Close()

	// Re-opening the same file must not re-run migrations or error.
	s2, err := openSiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if v2 := userVersion(t, s2); v2 != v1 {
		t.Errorf("version changed on reopen: %d -> %d", v1, v2)
	}
}

func TestMigrationsApplyIncrementally(t *testing.T) {
	s := newStore(t)

	// Simulate a future v(N+1) migration and confirm only it runs.
	next := len(siteMigrations) + 1
	migs := append([]migration{}, siteMigrations...)
	migs = append(migs, migration{
		version: next,
		stmts:   []string{`CREATE TABLE probe (id INTEGER PRIMARY KEY)`},
	})

	if err := applyMigrations(s.db, migs); err != nil {
		t.Fatalf("apply incremental: %v", err)
	}
	if got := userVersion(t, s); got != next {
		t.Fatalf("user_version = %d, want %d", got, next)
	}
	// The new table should exist and be usable.
	if _, err := s.db.Exec(`INSERT INTO probe DEFAULT VALUES`); err != nil {
		t.Errorf("probe table not created: %v", err)
	}
}
