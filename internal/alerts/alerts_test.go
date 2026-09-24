package alerts

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// recorder is a Sender that captures messages instead of sending them, and can
// be told to fail a number of times first.
type recorder struct {
	mu       sync.Mutex
	sent     []recorded
	failNext int
	failErr  error
}

type recorded struct{ To, Subject, Body string }

func (r *recorder) Send(_ context.Context, to, subject, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext > 0 {
		r.failNext--
		if r.failErr == nil {
			r.failErr = errors.New("smtp unavailable")
		}
		return r.failErr
	}
	r.sent = append(r.sent, recorded{to, subject, body})
	return nil
}

func (r *recorder) messages() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recorded, len(r.sent))
	copy(out, r.sent)
	return out
}

// harness wires a registry, a recorder, and a dispatcher over a temp database.
type harness struct {
	reg     *storage.Registry
	rec     *recorder
	disp    *Dispatcher
	site    config.SiteConfig
	channel storage.AlertChannel
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	reg, err := storage.OpenRegistry(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = time.Millisecond // keep retry tests fast
	}
	d := NewDispatcher(reg, rec, opts)
	t.Cleanup(func() {
		d.Shutdown()
		reg.Close()
	})

	site := config.SiteConfig{ID: "s1", Name: "Site One", URL: "https://example.com"}
	site.Checks.SSL.WarnDays = 14
	if err := reg.Create(site); err != nil {
		t.Fatal(err)
	}
	ch, err := reg.CreateAlertChannel(storage.AlertChannel{
		Name: "ops", Type: "email", Target: "ops@example.com", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{reg: reg, rec: rec, disp: d, site: site, channel: ch}
}

// addRule adds a rule of kind against the harness's single channel — the normal
// shape, where one destination has rules for several kinds.
func (h *harness) addRule(t *testing.T, kind string, confirmAfter int) (storage.AlertChannel, storage.AlertRule) {
	t.Helper()
	rule, err := h.reg.CreateAlertRule(storage.AlertRule{
		ChannelID: h.channel.ID, Kind: kind, Enabled: true, ConfirmAfter: confirmAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h.channel, rule
}

// fire submits an event and waits for the worker to finish with it.
func (h *harness) fire(t *testing.T, tr storage.Transition) {
	t.Helper()
	h.disp.handle(Event{Site: h.site, Transition: tr, At: time.Now()})
}

func TestDownAlertHeldUntilConfirmed(t *testing.T) {
	h := newHarness(t, Options{})
	h.addRule(t, storage.AlertSiteDown, 2)

	// First failure: an incident opens, but one blip must not send mail.
	h.fire(t, storage.Transition{IncidentOpened: true, IncidentID: 1,
		ConsecutiveFailures: 1, Up: false, Error: "timeout"})
	if n := len(h.rec.messages()); n != 0 {
		t.Fatalf("sent %d messages after one failure, want 0", n)
	}

	// Second consecutive failure reaches the threshold.
	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 2,
		Up: false, Error: "timeout"})
	msgs := h.rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages after confirmation, want 1", len(msgs))
	}
	if msgs[0].Subject != "[DOWN] Site One" {
		t.Errorf("subject = %q", msgs[0].Subject)
	}
	if msgs[0].To != "ops@example.com" {
		t.Errorf("to = %q", msgs[0].To)
	}
}

func TestDownAlertSentOncePerIncident(t *testing.T) {
	h := newHarness(t, Options{})
	h.addRule(t, storage.AlertSiteDown, 1)

	// Ten failing ticks in one outage must produce exactly one email.
	for i := 1; i <= 10; i++ {
		h.fire(t, storage.Transition{IncidentID: 7, ConsecutiveFailures: i,
			Up: false, Error: "timeout"})
	}
	if n := len(h.rec.messages()); n != 1 {
		t.Errorf("sent %d messages for one incident, want 1", n)
	}
}

// TestDedupeSurvivesRestart is the property a process-local cache would miss.
func TestDedupeSurvivesRestart(t *testing.T) {
	h := newHarness(t, Options{})
	h.addRule(t, storage.AlertSiteDown, 1)
	h.fire(t, storage.Transition{IncidentID: 3, ConsecutiveFailures: 1, Up: false})

	// A second dispatcher over the same database stands in for a restart.
	rec2 := &recorder{}
	d2 := NewDispatcher(h.reg, rec2, Options{RetryDelay: time.Millisecond})
	defer d2.Shutdown()
	d2.handle(Event{Site: h.site, At: time.Now(), Transition: storage.Transition{
		IncidentID: 3, ConsecutiveFailures: 2, Up: false,
	}})

	if n := len(rec2.messages()); n != 0 {
		t.Errorf("re-announced an outage after restart: %d messages", n)
	}
}

func TestRecoveryOnlyAfterAnnouncedOutage(t *testing.T) {
	h := newHarness(t, Options{})
	h.addRule(t, storage.AlertSiteDown, 2)
	h.addRule(t, storage.AlertSiteRecovered, 1)

	// A blip that recovers before confirmation: neither mail should be sent,
	// because an unannounced outage has nothing to recover from.
	h.fire(t, storage.Transition{IncidentOpened: true, IncidentID: 1,
		ConsecutiveFailures: 1, Up: false})
	h.fire(t, storage.Transition{IncidentResolved: true, IncidentID: 1, Up: true})
	if n := len(h.rec.messages()); n != 0 {
		t.Fatalf("blip produced %d messages, want 0", n)
	}

	// A confirmed outage that then recovers: both.
	h.fire(t, storage.Transition{IncidentOpened: true, IncidentID: 2,
		ConsecutiveFailures: 1, Up: false})
	h.fire(t, storage.Transition{IncidentID: 2, ConsecutiveFailures: 2, Up: false})
	h.fire(t, storage.Transition{IncidentResolved: true, IncidentID: 2, Up: true})

	msgs := h.rec.messages()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (down + recovered)", len(msgs))
	}
	if msgs[0].Subject != "[DOWN] Site One" || msgs[1].Subject != "[RECOVERED] Site One" {
		t.Errorf("subjects = %q, %q", msgs[0].Subject, msgs[1].Subject)
	}
}

func TestSSLExpiryAlertRespectsWarnDays(t *testing.T) {
	h := newHarness(t, Options{})
	h.addRule(t, storage.AlertSSLExpiring, 1)

	// 60 days out with a 14-day warning: silent.
	far := time.Now().Add(60 * 24 * time.Hour).Unix()
	h.fire(t, storage.Transition{Up: true, SSLChecked: true, SSLExpiresAt: far})
	if n := len(h.rec.messages()); n != 0 {
		t.Fatalf("alerted %d times for a cert 60 days out, want 0", n)
	}

	// 5 days out: alert, once, however often the slow check runs.
	near := time.Now().Add(5 * 24 * time.Hour).Unix()
	for i := 0; i < 3; i++ {
		h.fire(t, storage.Transition{Up: true, SSLChecked: true, SSLExpiresAt: near})
	}
	msgs := h.rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if want := "[SSL] Site One certificate expires in 4 day(s)"; msgs[0].Subject != want {
		t.Errorf("subject = %q, want %q", msgs[0].Subject, want)
	}

	// A renewed certificate has a new expiry, so it is a new dedupe key and can
	// alert again on its own terms.
	renewed := time.Now().Add(3 * 24 * time.Hour).Unix()
	h.fire(t, storage.Transition{Up: true, SSLChecked: true, SSLExpiresAt: renewed})
	if n := len(h.rec.messages()); n != 2 {
		t.Errorf("a different expiry should alert again: got %d, want 2", n)
	}
}

func TestDisabledChannelSilencesRule(t *testing.T) {
	h := newHarness(t, Options{})
	ch, _ := h.addRule(t, storage.AlertSiteDown, 1)
	ch.Enabled = false
	if err := h.reg.UpdateAlertChannel(ch); err != nil {
		t.Fatal(err)
	}
	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 1, Up: false})
	if n := len(h.rec.messages()); n != 0 {
		t.Errorf("disabled channel still sent %d messages", n)
	}
}

func TestRuleScopedToOtherSiteDoesNotFire(t *testing.T) {
	h := newHarness(t, Options{})
	other := config.SiteConfig{ID: "s2", Name: "Other", URL: "https://other.example.com"}
	if err := h.reg.Create(other); err != nil {
		t.Fatal(err)
	}
	ch, rule := h.addRule(t, storage.AlertSiteDown, 1)
	id := "s2"
	rule.SiteID = &id
	rule.ChannelID = ch.ID
	if err := h.reg.UpdateAlertRule(rule); err != nil {
		t.Fatal(err)
	}
	// The event is for s1; the rule names s2.
	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 1, Up: false})
	if n := len(h.rec.messages()); n != 0 {
		t.Errorf("rule for another site fired: %d messages", n)
	}
}

func TestRetryThenSucceedRecordsAttempts(t *testing.T) {
	h := newHarness(t, Options{Retries: 3, RetryDelay: time.Millisecond})
	h.addRule(t, storage.AlertSiteDown, 1)
	h.rec.failNext = 2

	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 1, Up: false})

	if n := len(h.rec.messages()); n != 1 {
		t.Fatalf("got %d messages, want 1 after retries", n)
	}
	dels, err := h.reg.ListAlertDeliveries("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(dels))
	}
	if dels[0].Status != storage.DeliverySent {
		t.Errorf("status = %q, want sent", dels[0].Status)
	}
	if dels[0].Attempts != 3 {
		t.Errorf("attempts = %d, want 3", dels[0].Attempts)
	}
}

func TestPermanentFailureRecordedAndNotRetriedForever(t *testing.T) {
	h := newHarness(t, Options{Retries: 1, RetryDelay: time.Millisecond})
	h.addRule(t, storage.AlertSiteDown, 1)
	h.rec.failNext = 1000 // never succeeds

	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 1, Up: false})

	dels, err := h.reg.ListAlertDeliveries("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 1 || dels[0].Status != storage.DeliveryFailed {
		t.Fatalf("deliveries = %+v, want one failed", dels)
	}
	if dels[0].Error == nil || *dels[0].Error == "" {
		t.Error("failure cause was not recorded")
	}

	// The row stays claimed, so a broken channel does not become a mail loop
	// retrying on every subsequent tick.
	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 2, Up: false})
	dels, _ = h.reg.ListAlertDeliveries("", 10)
	if len(dels) != 1 {
		t.Errorf("got %d deliveries, want 1 — a failed send must not re-claim", len(dels))
	}
}

func TestEnqueueNeverBlocks(t *testing.T) {
	h := newHarness(t, Options{QueueSize: 1, Workers: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			h.disp.Enqueue(Event{Site: h.site, At: time.Now()})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked; the check loop would have stalled")
	}
	if h.disp.Dropped() == 0 {
		t.Log("note: no events dropped (workers kept up)")
	}
}

func TestNoRulesMeansNoDeliveries(t *testing.T) {
	h := newHarness(t, Options{})
	h.fire(t, storage.Transition{IncidentID: 1, ConsecutiveFailures: 5, Up: false})
	if n := len(h.rec.messages()); n != 0 {
		t.Errorf("sent %d messages with no rules configured", n)
	}
}

func TestSanitiseHeaderStripsInjection(t *testing.T) {
	// A site name is admin-supplied and lands in the Subject header.
	got := sanitiseHeader("Site\r\nBcc: attacker@example.com")
	if got != "Site Bcc: attacker@example.com" {
		t.Errorf("sanitiseHeader = %q", got)
	}
}
