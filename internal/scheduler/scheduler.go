// Package scheduler runs one goroutine per enabled site, firing checks at the
// site's configured interval and persisting each result.
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/stuart/uptime-monitor/internal/checker"
	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// TransitionFunc is notified after every recorded tick, with what changed.
//
// It is called on the site's check goroutine, so it must return promptly —
// the alert dispatcher satisfies this by queueing and returning. Declaring the
// hook as a function keeps scheduler independent of internal/alerts.
type TransitionFunc func(site config.SiteConfig, tr storage.Transition)

// Manager supervises per-site check loops. It is safe for concurrent use; the
// API calls Start/Stop/Restart as sites are created, updated, or deleted.
type Manager struct {
	stores *storage.Manager
	runner *checker.Runner

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	onTrans TransitionFunc
	wg      sync.WaitGroup
}

// NewManager creates a scheduler manager.
func NewManager(stores *storage.Manager, runner *checker.Runner) *Manager {
	return &Manager{
		stores:  stores,
		runner:  runner,
		cancels: map[string]context.CancelFunc{},
	}
}

// OnTransition registers the post-tick hook. Safe to call before StartAll; a
// nil hook means transitions are simply not reported.
func (m *Manager) OnTransition(f TransitionFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onTrans = f
}

// transitionHook returns the current hook under lock.
func (m *Manager) transitionHook() TransitionFunc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.onTrans
}

// StartAll launches loops for every enabled site.
func (m *Manager) StartAll(sites []config.SiteConfig) {
	for _, s := range sites {
		if s.Enabled {
			m.Start(s)
		}
	}
}

// Start launches (or restarts) the loop for a site.
func (m *Manager) Start(site config.SiteConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(site.ID)

	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[site.ID] = cancel
	m.wg.Add(1)
	go m.runSite(ctx, site)
}

// Stop halts the loop for a site id (no-op if not running).
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(id)
}

// Restart applies a new config by stopping and restarting the loop.
func (m *Manager) Restart(site config.SiteConfig) {
	m.Stop(site.ID)
	if site.Enabled {
		m.Start(site)
	}
}

func (m *Manager) stopLocked(id string) {
	if cancel, ok := m.cancels[id]; ok {
		cancel()
		delete(m.cancels, id)
	}
}

// Shutdown stops all loops and waits for them to finish.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	for id, cancel := range m.cancels {
		cancel()
		delete(m.cancels, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// runSite is the per-site check loop.
func (m *Manager) runSite(ctx context.Context, site config.SiteConfig) {
	defer m.wg.Done()

	store, err := m.stores.Get(site.ID)
	if err != nil {
		log.Printf("scheduler: cannot open store for %s: %v", site.ID, err)
		return
	}

	interval := time.Duration(site.IntervalSeconds) * time.Second
	slow := time.Duration(site.SlowIntervalSeconds) * time.Second

	// lastSlow zero-value means the first tick runs the slow checks too.
	var lastSlow time.Time

	tick := func() {
		// A panic in one site's check must not take down the whole daemon.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("scheduler: recovered panic in check for %s: %v", site.ID, r)
			}
		}()
		runSlow := time.Since(lastSlow) >= slow
		result := m.runner.RunTick(ctx, site, runSlow)
		if runSlow {
			lastSlow = time.Now()
		}
		tr, err := store.RecordTick(result)
		if err != nil {
			log.Printf("scheduler: record tick for %s: %v", site.ID, err)
			return
		}
		// Notify only after the write has committed: a hook that acted on an
		// uncommitted transition could announce an outage the database rolled back.
		if hook := m.transitionHook(); hook != nil {
			hook(site, tr)
		}
	}

	// Run once immediately so there's data without waiting a full interval.
	tick()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
