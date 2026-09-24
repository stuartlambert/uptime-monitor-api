package auth

import (
	"sync"
	"time"
)

// Default throttling for login attempts.
const (
	DefaultMaxAttempts = 5
	DefaultWindow      = 15 * time.Minute
)

// Limiter throttles login attempts per key. Two keys are registered for every
// attempt — the username and the client IP — so neither password-spraying one
// account nor trying many accounts from one address gets a free pass.
//
// State is in memory only: a restart clears it. That is an acceptable trade for
// a single-node service, and it is documented in docs/ROLLOUT.md as the
// lockout escape hatch.
type Limiter struct {
	maxAttempts int
	window      time.Duration

	mu       sync.Mutex
	attempts map[string][]time.Time

	// now is swappable so tests need not sleep.
	now func() time.Time
}

// NewLimiter builds a limiter. Zero values select the defaults.
func NewLimiter(maxAttempts int, window time.Duration) *Limiter {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if window <= 0 {
		window = DefaultWindow
	}
	return &Limiter{
		maxAttempts: maxAttempts,
		window:      window,
		attempts:    map[string][]time.Time{},
		now:         time.Now,
	}
}

// Allowed reports whether every key is currently under the limit. It does not
// record anything — call RecordFailure after an attempt actually fails.
func (l *Limiter) Allowed(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-l.window)
	for _, k := range keys {
		if len(l.liveLocked(k, cutoff)) >= l.maxAttempts {
			return false
		}
	}
	return true
}

// RecordFailure counts a failed attempt against every key.
func (l *Limiter) RecordFailure(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	for _, k := range keys {
		l.attempts[k] = append(l.liveLocked(k, cutoff), now)
	}
}

// Reset clears the counters for keys. Called on a successful login so a user who
// mistypes twice and then succeeds is not left near the limit.
func (l *Limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.attempts, k)
	}
}

// RetryAfter returns how long until the oldest attempt for key ages out, or 0
// when the key is not currently limited.
func (l *Limiter) RetryAfter(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	var longest time.Duration
	for _, k := range keys {
		live := l.liveLocked(k, cutoff)
		if len(live) < l.maxAttempts {
			continue
		}
		if d := live[0].Add(l.window).Sub(now); d > longest {
			longest = d
		}
	}
	return longest
}

// liveLocked returns (and prunes) the attempts for key newer than cutoff.
// Pruning on read keeps the map from growing without a background sweep.
func (l *Limiter) liveLocked(key string, cutoff time.Time) []time.Time {
	ts := l.attempts[key]
	i := 0
	for ; i < len(ts); i++ {
		if ts[i].After(cutoff) {
			break
		}
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.attempts, key)
	} else {
		l.attempts[key] = ts
	}
	return ts
}
