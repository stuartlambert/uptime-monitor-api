// Package alerts turns monitoring state changes into notifications.
//
// The scheduler hands it an Event after each check and returns immediately;
// everything else — matching rules, suppressing duplicates, sending mail, and
// retrying — happens on this package's own goroutines. That separation is the
// point: a check loop must never wait on an SMTP server.
package alerts

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// Event is one monitoring state change worth considering for an alert.
type Event struct {
	Site       config.SiteConfig
	Transition storage.Transition
	At         time.Time
}

// Sender delivers a rendered message. Implemented by SMTPSender in production
// and by a recorder in tests, so the dispatcher is testable without a mail
// server.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Dispatcher consumes Events and delivers the alerts they warrant.
type Dispatcher struct {
	reg    *storage.Registry
	sender Sender

	queue chan Event
	wg    sync.WaitGroup

	// sendTimeout bounds one delivery attempt.
	sendTimeout time.Duration
	// retries is how many additional attempts a failed send gets.
	retries int
	// retryDelay is the base backoff, doubled per attempt.
	retryDelay time.Duration

	stopOnce sync.Once
	done     chan struct{}

	// dropped counts events discarded because the queue was full.
	mu      sync.Mutex
	dropped int64
}

// Options tunes a Dispatcher. Zero values select sensible defaults.
type Options struct {
	QueueSize   int
	Workers     int
	SendTimeout time.Duration
	Retries     int
	RetryDelay  time.Duration
}

func (o Options) withDefaults() Options {
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = 30 * time.Second
	}
	if o.Retries < 0 {
		o.Retries = 0
	}
	if o.Retries == 0 {
		o.Retries = 2
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 5 * time.Second
	}
	return o
}

// NewDispatcher starts the worker pool. Call Shutdown to drain it.
func NewDispatcher(reg *storage.Registry, sender Sender, opts Options) *Dispatcher {
	opts = opts.withDefaults()
	d := &Dispatcher{
		reg:         reg,
		sender:      sender,
		queue:       make(chan Event, opts.QueueSize),
		sendTimeout: opts.SendTimeout,
		retries:     opts.Retries,
		retryDelay:  opts.RetryDelay,
		done:        make(chan struct{}),
	}
	for i := 0; i < opts.Workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// Enqueue submits an event without blocking.
//
// If the queue is full the event is dropped and counted. Dropping alerts is bad;
// blocking the scheduler is worse, because that stops the monitoring the alerts
// are about. The drop is logged so the loss is never silent.
func (d *Dispatcher) Enqueue(e Event) {
	select {
	case d.queue <- e:
	default:
		d.mu.Lock()
		d.dropped++
		n := d.dropped
		d.mu.Unlock()
		log.Printf("alerts: queue full, dropped event for %s (%d dropped total)", e.Site.ID, n)
	}
}

// Dropped reports how many events have been discarded for lack of queue space.
func (d *Dispatcher) Dropped() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

// Shutdown stops accepting events and waits for in-flight deliveries.
func (d *Dispatcher) Shutdown() {
	d.stopOnce.Do(func() {
		close(d.done)
		close(d.queue)
	})
	d.wg.Wait()
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for e := range d.queue {
		// One bad event must not take the pool down with it.
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("alerts: recovered panic handling event for %s: %v", e.Site.ID, r)
				}
			}()
			d.handle(e)
		}()
	}
}

// handle decides which alerts an event warrants and delivers them.
func (d *Dispatcher) handle(e Event) {
	for _, k := range kindsFor(e) {
		rules, err := d.reg.RulesFor(e.Site.ID, k)
		if err != nil {
			log.Printf("alerts: load rules for %s/%s: %v", e.Site.ID, k, err)
			continue
		}
		for _, rule := range rules {
			d.applyRule(e, k, rule)
		}
	}
}

// kindsFor reports which alert kinds this transition could trigger. Whether one
// is actually sent still depends on the rule's threshold and on deduplication.
func kindsFor(e Event) []string {
	var kinds []string
	tr := e.Transition
	if !tr.Up && tr.ConsecutiveFailures > 0 {
		kinds = append(kinds, storage.AlertSiteDown)
	}
	if tr.IncidentResolved {
		kinds = append(kinds, storage.AlertSiteRecovered)
	}
	if tr.SSLChecked && tr.SSLExpiresAt > 0 {
		kinds = append(kinds, storage.AlertSSLExpiring)
	}
	return kinds
}

// applyRule evaluates one rule against an event and, if it fires, claims and
// sends the notification.
func (d *Dispatcher) applyRule(e Event, kind string, rule storage.AlertRule) {
	ch, err := d.reg.GetAlertChannel(rule.ChannelID)
	if err != nil {
		log.Printf("alerts: load channel %d: %v", rule.ChannelID, err)
		return
	}

	msg, key, ok := d.render(e, kind, rule, ch)
	if !ok {
		return
	}

	var incID *int64
	if e.Transition.IncidentID != 0 {
		v := e.Transition.IncidentID
		incID = &v
	}
	id, err := d.reg.ClaimDelivery(storage.AlertDelivery{
		DedupeKey:  key,
		SiteID:     e.Site.ID,
		Kind:       kind,
		ChannelID:  ch.ID,
		IncidentID: incID,
		Subject:    msg.Subject,
	})
	if err == storage.ErrDeliveryClaimed {
		return // already handled — the normal path on a repeat tick
	}
	if err != nil {
		log.Printf("alerts: claim delivery for %s/%s: %v", e.Site.ID, kind, err)
		return
	}

	d.deliver(id, ch, msg)
}

// deliver sends with bounded retries and records the outcome.
func (d *Dispatcher) deliver(deliveryID int64, ch storage.AlertChannel, msg message) {
	var lastErr error
	for attempt := 1; attempt <= d.retries+1; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), d.sendTimeout)
		err := d.sender.Send(ctx, ch.Target, msg.Subject, msg.Body)
		cancel()
		if err == nil {
			if err := d.reg.MarkDeliverySent(deliveryID, attempt); err != nil {
				log.Printf("alerts: mark delivery %d sent: %v", deliveryID, err)
			}
			log.Printf("alerts: sent %q to %s", msg.Subject, ch.Target)
			return
		}
		lastErr = err
		if attempt <= d.retries {
			backoff := d.retryDelay * time.Duration(1<<(attempt-1))
			select {
			case <-time.After(backoff):
			case <-d.done:
				// Shutting down: stop retrying, but still record the failure.
				attempt = d.retries + 1
			}
		}
	}
	log.Printf("alerts: giving up on %q to %s: %v", msg.Subject, ch.Target, lastErr)
	if err := d.reg.MarkDeliveryFailed(deliveryID, d.retries+1, lastErr.Error()); err != nil {
		log.Printf("alerts: mark delivery %d failed: %v", deliveryID, err)
	}
}

// message is a rendered notification.
type message struct {
	Subject string
	Body    string
}

// render builds the message for a firing rule and its dedupe key. The bool
// reports whether the rule fires at all — thresholds and suppression live here.
func (d *Dispatcher) render(e Event, kind string, rule storage.AlertRule, ch storage.AlertChannel) (message, string, bool) {
	site := e.Site
	tr := e.Transition
	when := e.At.UTC().Format("2006-01-02 15:04:05 UTC")

	switch kind {
	case storage.AlertSiteDown:
		// Hold until the failure is confirmed, so a single blip sends nothing.
		// Fire only on the exact tick that reaches the threshold: later ticks in
		// the same outage would be deduped anyway, but this avoids the work.
		confirm := rule.ConfirmAfter
		if confirm < 1 {
			confirm = 1
		}
		if tr.ConsecutiveFailures != confirm {
			return message{}, "", false
		}
		key := fmt.Sprintf("down:%s:%d:%d", site.ID, tr.IncidentID, ch.ID)
		subject := fmt.Sprintf("[DOWN] %s", site.Name)
		body := fmt.Sprintf(""+
			"%s is DOWN.\n\n"+
			"URL:      %s\n"+
			"Detected: %s\n"+
			"Failures: %d consecutive\n"+
			"Error:    %s\n",
			site.Name, site.URL, when, tr.ConsecutiveFailures, orDash(tr.Error))
		return message{subject, body}, key, true

	case storage.AlertSiteRecovered:
		// Only announce recovery for an outage that was announced. Otherwise a
		// blip that never earned a DOWN mail still sends an "UP" one, which
		// reads as a message about an outage you were never told about.
		//
		// The check is per channel, so each destination sees a consistent story:
		// a channel that was never told a site went down is never told it came
		// back. The consequence is that a channel with only a site_recovered
		// rule stays silent — pair it with a site_down rule on the same channel.
		downKey := fmt.Sprintf("down:%s:%d:%d", site.ID, tr.IncidentID, ch.ID)
		announced, err := d.reg.DeliveryExists(downKey)
		if err != nil {
			log.Printf("alerts: check prior down alert: %v", err)
			return message{}, "", false
		}
		if !announced {
			return message{}, "", false
		}
		key := fmt.Sprintf("up:%s:%d:%d", site.ID, tr.IncidentID, ch.ID)
		subject := fmt.Sprintf("[RECOVERED] %s", site.Name)
		body := fmt.Sprintf(""+
			"%s is back UP.\n\n"+
			"URL:       %s\n"+
			"Recovered: %s\n",
			site.Name, site.URL, when)
		return message{subject, body}, key, true

	case storage.AlertSSLExpiring:
		warn := site.Checks.SSL.WarnDays
		if warn <= 0 {
			warn = config.DefaultSSLWarnDays
		}
		expiry := time.Unix(tr.SSLExpiresAt, 0).UTC()
		daysLeft := int(time.Until(expiry).Hours() / 24)
		if daysLeft > warn {
			return message{}, "", false
		}
		// Keyed on the expiry date, so one mail per certificate rather than one
		// per hourly check — and a renewed certificate alerts again on its own
		// terms rather than being suppressed by the old one's key.
		key := fmt.Sprintf("ssl:%s:%d:%d", site.ID, tr.SSLExpiresAt, ch.ID)
		subject := fmt.Sprintf("[SSL] %s certificate expires in %d day(s)", site.Name, daysLeft)
		body := fmt.Sprintf(""+
			"The TLS certificate for %s expires soon.\n\n"+
			"URL:     %s\n"+
			"Expires: %s (%d day(s))\n"+
			"Checked: %s\n",
			site.Name, site.URL, expiry.Format("2006-01-02 15:04:05 UTC"), daysLeft, when)
		return message{subject, body}, key, true
	}
	return message{}, "", false
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
