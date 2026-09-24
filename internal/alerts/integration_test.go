package alerts_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stuart/uptime-monitor/internal/alerts"
	"github.com/stuart/uptime-monitor/internal/checker"
	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/scheduler"
	"github.com/stuart/uptime-monitor/internal/storage"
)

type capture struct {
	mu   sync.Mutex
	subs []string
	got  chan struct{}
}

func (c *capture) Send(_ context.Context, _, subject, _ string) error {
	c.mu.Lock()
	c.subs = append(c.subs, subject)
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
	return nil
}

func (c *capture) subjects() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.subs...)
}

// TestSchedulerToEmail runs the real path: a failing HTTP backend, the real
// checker and scheduler, RecordTick, the transition hook, and the dispatcher.
// The unit tests all inject synthetic transitions; this is the one that proves
// they are wired to each other.
func TestSchedulerToEmail(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer down.Close()

	dir := t.TempDir()
	stores, err := storage.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close()
	reg, err := storage.OpenRegistry(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	site := config.SiteConfig{
		ID: "flaky", Name: "Flaky", URL: down.URL,
		IntervalSeconds: 1, SlowIntervalSeconds: 3600, Enabled: true,
	}
	site.Checks.HTTP.Enabled = true
	if err := reg.Create(site); err != nil {
		t.Fatal(err)
	}

	ch, err := reg.CreateAlertChannel(storage.AlertChannel{
		Name: "ops", Type: "email", Target: "ops@example.com", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateAlertRule(storage.AlertRule{
		ChannelID: ch.ID, Kind: storage.AlertSiteDown, Enabled: true, ConfirmAfter: 2,
	}); err != nil {
		t.Fatal(err)
	}

	cap := &capture{got: make(chan struct{}, 4)}
	disp := alerts.NewDispatcher(reg, cap, alerts.Options{RetryDelay: time.Millisecond})
	defer disp.Shutdown()

	sched := scheduler.NewManager(stores, checker.NewRunner(2*time.Second, false))
	sched.OnTransition(func(s config.SiteConfig, tr storage.Transition) {
		disp.Enqueue(alerts.Event{Site: s, Transition: tr, At: time.Now()})
	})
	sched.Start(site)
	defer sched.Shutdown()

	select {
	case <-cap.got:
	case <-time.After(15 * time.Second):
		t.Fatal("no alert produced by a persistently failing site")
	}

	subs := cap.subjects()
	if len(subs) == 0 || subs[0] != "[DOWN] Flaky" {
		t.Fatalf("subjects = %v, want first to be [DOWN] Flaky", subs)
	}

	// The counter must have been persisted, since confirm_after depends on it
	// surviving between ticks.
	store, err := stores.Get("flaky")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Up == nil || *st.Up {
		t.Error("site should be recorded as down")
	}
}
