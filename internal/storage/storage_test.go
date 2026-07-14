package storage

import (
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *SiteStore {
	t.Helper()
	s, err := openSiteStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func up(ts int64) Tick { return Tick{TS: ts, Up: true, StatusCode: 200, ResponseMs: 100} }
func down(ts int64) Tick {
	tk := Tick{TS: ts, StatusCode: 500, ResponseMs: 50}
	tk.AddFail("http", "unexpected status 500")
	return tk
}

func TestIncidentOpenAndResolve(t *testing.T) {
	s := newStore(t)

	if err := s.RecordTick(up(1000)); err != nil {
		t.Fatal(err)
	}
	// Two consecutive failures: a single incident should open and stay open.
	if err := s.RecordTick(down(1060)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTick(down(1120)); err != nil {
		t.Fatal(err)
	}
	incs, err := s.Incidents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incs))
	}
	if incs[0].ResolvedAt != nil {
		t.Error("incident should still be open")
	}

	// Recovery resolves it.
	if err := s.RecordTick(up(1180)); err != nil {
		t.Fatal(err)
	}
	incs, _ = s.Incidents(10)
	if incs[0].ResolvedAt == nil {
		t.Fatal("incident should be resolved")
	}
	if got := *incs[0].DurationS; got != 120 {
		t.Errorf("duration = %d, want 120", got)
	}

	st, _ := s.Status()
	if st.Up == nil || !*st.Up {
		t.Error("state should report up after recovery")
	}
	if st.OngoingIncidentID != nil {
		t.Error("no incident should be ongoing after recovery")
	}
}

func TestUptimePercent(t *testing.T) {
	s := newStore(t)
	// 3 up, 1 down => 75%.
	for _, tk := range []Tick{up(1), up(2), down(3), up(4)} {
		if err := s.RecordTick(tk); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.Uptime("all", 0)
	if err != nil {
		t.Fatal(err)
	}
	if u.Checks != 4 || u.Successful != 3 || u.Failed != 1 {
		t.Errorf("counts = %+v", u)
	}
	if u.Percent != 75 {
		t.Errorf("uptime = %.1f, want 75", u.Percent)
	}
}

func TestErrorsLogged(t *testing.T) {
	s := newStore(t)
	if err := s.RecordTick(down(500)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Errors(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CheckType != "http" {
		t.Fatalf("expected 1 http error, got %+v", rows)
	}
}

func TestPercentiles(t *testing.T) {
	s := newStore(t)
	// response_ms 1..100 on successful checks.
	for i := int64(1); i <= 100; i++ {
		if err := s.RecordTick(Tick{TS: i, Up: true, StatusCode: 200, ResponseMs: i}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.Metrics("all", 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Checks != 100 {
		t.Errorf("checks = %d, want 100", m.Checks)
	}
	// p50 offset = 50 -> the 51st smallest value = 51.
	if m.P50Ms != 51 {
		t.Errorf("p50 = %d, want 51", m.P50Ms)
	}
	// p95 offset = 95 -> 96.
	if m.P95Ms != 96 {
		t.Errorf("p95 = %d, want 96", m.P95Ms)
	}
	if m.AvgMs < 50 || m.AvgMs > 51 {
		t.Errorf("avg = %.2f, want ~50.5", m.AvgMs)
	}
}

func TestRegistryCRUD(t *testing.T) {
	reg, err := OpenRegistry(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	// Uses the config package indirectly via the storage API surface.
	sites, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 0 {
		t.Errorf("expected empty registry, got %d", len(sites))
	}
	if _, err := reg.Get("missing"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
