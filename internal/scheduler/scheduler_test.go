package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stuart/uptime-monitor/internal/checker"
	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/storage"
)

func testSite(url string) config.SiteConfig {
	s := config.SiteConfig{ID: "s1", Name: "s1", URL: url, Enabled: true}
	s.Checks.HTTP.Enabled = true
	s.ApplyDefaults()
	s.IntervalSeconds = config.MinIntervalSeconds // fast as allowed
	return s
}

func newMgr(t *testing.T) (*Manager, *storage.Manager) {
	t.Helper()
	stores, err := storage.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stores.Close)
	return NewManager(stores, checker.NewRunner(2*time.Second, false)), stores
}

// TestStartRecordsTick verifies the loop runs an immediate check on Start.
func TestStartRecordsTick(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	mgr, stores := newMgr(t)
	site := testSite(backend.URL)
	mgr.Start(site)
	defer mgr.Shutdown()

	store, err := stores.Get(site.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The first tick fires immediately; poll briefly for the row.
	if !waitFor(2*time.Second, func() bool {
		rows, _ := store.Results(0, 10)
		return len(rows) >= 1
	}) {
		t.Fatal("no check result recorded within timeout")
	}
	rows, _ := store.Results(0, 10)
	if !rows[0].Up {
		t.Errorf("expected up result, got %+v", rows[0])
	}
}

// TestStopHaltsLoop verifies Stop ends the loop (no further rows accrue).
func TestStopHaltsLoop(t *testing.T) {
	var hits int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	defer backend.Close()

	mgr, _ := newMgr(t)
	site := testSite(backend.URL)
	mgr.Start(site)

	// Let the immediate tick land, then stop.
	waitFor(2*time.Second, func() bool { return atomic.LoadInt64(&hits) >= 1 })
	mgr.Stop(site.ID)

	after := atomic.LoadInt64(&hits)
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt64(&hits); got != after {
		t.Errorf("loop kept running after Stop: %d -> %d", after, got)
	}
}

// TestRestartDisables verifies Restart with Enabled=false stops the loop.
func TestRestartDisables(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	mgr, _ := newMgr(t)
	site := testSite(backend.URL)
	mgr.Start(site)
	defer mgr.Shutdown()

	site.Enabled = false
	mgr.Restart(site) // should stop and not restart

	mgr.mu.Lock()
	_, running := mgr.cancels[site.ID]
	mgr.mu.Unlock()
	if running {
		t.Error("expected no running loop after disabling restart")
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
