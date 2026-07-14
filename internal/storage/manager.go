package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Manager owns the per-site database files. Each site gets its own SQLite file
// at <dir>/<id>.db, opened lazily and cached for reuse.
type Manager struct {
	dir string

	mu    sync.Mutex
	cache map[string]*SiteStore
}

// NewManager creates a Manager rooted at dir, creating the directory if needed.
func NewManager(dir string) (*Manager, error) {
	// 0750: the data files hold check history and error text; keep them off
	// limits to other local users on a shared host.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Manager{dir: dir, cache: map[string]*SiteStore{}}, nil
}

// path returns the database file path for a site id.
func (m *Manager) path(id string) string {
	return filepath.Join(m.dir, id+".db")
}

// safeID rejects ids that could escape the data directory. Ids reaching here
// are already slug-validated at the config layer; this is defence in depth so a
// path-traversal id can never touch the filesystem even if validation regresses.
func safeID(id string) bool {
	return id != "" && id != "." && id != ".." &&
		id == filepath.Base(id) && !strings.ContainsAny(id, `/\`)
}

// Get returns the SiteStore for id, opening it on first use.
func (m *Manager) Get(id string) (*SiteStore, error) {
	if !safeID(id) {
		return nil, fmt.Errorf("invalid site id %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.cache[id]; ok {
		return s, nil
	}
	s, err := openSiteStore(m.path(id))
	if err != nil {
		return nil, err
	}
	m.cache[id] = s
	return s, nil
}

// Delete closes and removes a site's database file (and its WAL/SHM sidecars).
func (m *Manager) Delete(id string) error {
	if !safeID(id) {
		return fmt.Errorf("invalid site id %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.cache[id]; ok {
		s.Close()
		delete(m.cache, id)
	}
	base := m.path(id)
	var firstErr error
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close closes all open site databases.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.cache {
		s.Close()
		delete(m.cache, id)
	}
}
