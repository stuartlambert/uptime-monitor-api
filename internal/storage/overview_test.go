package storage

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"
)

func newRegAndStores(t *testing.T) (*Registry, *Manager) {
	t.Helper()
	dir := t.TempDir()
	stores, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := OpenRegistry(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close(); stores.Close() })
	return reg, stores
}

func TestOverviewCombinesConfigStateAndUptime(t *testing.T) {
	reg, stores := newRegAndStores(t)
	for _, id := range []string{"a", "b"} {
		s := config.SiteConfig{ID: id, Name: id, URL: "https://" + id + ".example.com"}
		if err := reg.Create(s); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	sa, _ := stores.Get("a")
	// 3 up, 1 down => 75%.
	for i, up := range []bool{true, true, false, true} {
		tk := Tick{TS: now - int64(4-i), Up: up, StatusCode: 200, ResponseMs: 100}
		if !up {
			tk.StatusCode = 500
			tk.AddFail("http", "unexpected status 500")
		}
		if _, err := sa.RecordTick(tk); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := Overview(reg, stores, "all", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	var a *SiteOverview
	for i := range rows {
		if rows[i].Site.ID == "a" {
			a = &rows[i]
		}
	}
	if a == nil {
		t.Fatal("site a missing")
	}
	if a.UptimePercent != 75 {
		t.Errorf("uptime = %v, want 75", a.UptimePercent)
	}
	if a.Checks != 4 || a.Failed != 1 {
		t.Errorf("checks/failed = %d/%d, want 4/1", a.Checks, a.Failed)
	}
	if a.Up == nil || !*a.Up {
		t.Error("site a should be up (last tick succeeded)")
	}
	if a.LastCheckTS == nil {
		t.Error("last_check_ts should be set")
	}
}

func TestOverviewNeverCheckedSite(t *testing.T) {
	reg, stores := newRegAndStores(t)
	if err := reg.Create(config.SiteConfig{
		ID: "fresh", Name: "Fresh", URL: "https://fresh.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := Overview(reg, stores, "24h", time.Now().Unix()-86400)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// Never checked: up is null rather than false, so the UI can distinguish
	// "not yet known" from "down".
	if rows[0].Up != nil {
		t.Errorf("up = %v, want nil for a site with no checks", *rows[0].Up)
	}
	if rows[0].Checks != 0 {
		t.Errorf("checks = %d, want 0", rows[0].Checks)
	}
	if rows[0].Error != "" {
		t.Errorf("unexpected error field: %q", rows[0].Error)
	}
}

func TestOverviewEmptyRegistry(t *testing.T) {
	reg, stores := newRegAndStores(t)
	rows, err := Overview(reg, stores, "all", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

// TestOverviewSerialisesAllFields guards the embedding trap: SiteConfig defines
// MarshalJSON, so embedding it would promote that method and the whole overview
// would serialise as just the site config.
func TestOverviewSerialisesAllFields(t *testing.T) {
	reg, stores := newRegAndStores(t)
	if err := reg.Create(config.SiteConfig{
		ID: "x", Name: "X", URL: "https://x.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ := Overview(reg, stores, "24h", time.Now().Unix()-86400)
	b, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"site", "up", "last_check_ts", "uptime_percent", "checks", "window"} {
		if _, ok := m[key]; !ok {
			t.Errorf("serialised overview is missing %q: %s", key, b)
		}
	}
	site, ok := m["site"].(map[string]any)
	if !ok {
		t.Fatalf("site is not an object: %s", b)
	}
	// The nested config keeps its own custom marshalling.
	if _, ok := site["has_api_key"]; !ok {
		t.Errorf("nested site config lost its has_api_key field: %s", b)
	}
}
