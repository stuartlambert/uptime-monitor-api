package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndCheckPassword(t *testing.T) {
	const pw = "correct-horse-battery-staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if h == pw || !strings.HasPrefix(h, "$2a$") {
		t.Fatalf("hash looks wrong: %q", h)
	}
	if !CheckPassword(h, pw) {
		t.Error("correct password rejected")
	}
	if CheckPassword(h, pw+"x") {
		t.Error("wrong password accepted")
	}
}

func TestHashPasswordSaltsPerCall(t *testing.T) {
	a, _ := HashPassword("correct-horse-battery-staple")
	b, _ := HashPassword("correct-horse-battery-staple")
	if a == b {
		t.Error("identical passwords produced identical hashes; salt is missing")
	}
}

func TestHashPasswordLengthLimits(t *testing.T) {
	if _, err := HashPassword("short"); err != ErrPasswordTooShort {
		t.Errorf("short password: got %v, want ErrPasswordTooShort", err)
	}
	// bcrypt truncates past 72 bytes, so anything longer must be refused rather
	// than silently collapsing to the same hash as its 72-byte prefix.
	if _, err := HashPassword(strings.Repeat("a", 73)); err != ErrPasswordTooLong {
		t.Errorf("long password: got %v, want ErrPasswordTooLong", err)
	}
	if _, err := HashPassword(strings.Repeat("a", 72)); err != nil {
		t.Errorf("72 bytes should be accepted: %v", err)
	}
}

func TestDummyHashIsUsableAndNeverMatches(t *testing.T) {
	// It must be a real bcrypt digest, or the timing equalisation it exists for
	// would not actually run the same work.
	if !strings.HasPrefix(DummyHash, "$2a$") {
		t.Fatalf("DummyHash is not a bcrypt digest: %q", DummyHash)
	}
	for _, guess := range []string{"", "password", "admin"} {
		if CheckPassword(DummyHash, guess) {
			t.Errorf("DummyHash matched %q", guess)
		}
	}
}

func TestSessionTokenIsRandomAndHashed(t *testing.T) {
	tok1, id1, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	tok2, id2, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok1 == tok2 || id1 == id2 {
		t.Error("session tokens repeat")
	}
	if len(tok1) < 40 {
		t.Errorf("token too short: %d chars", len(tok1))
	}
	// The stored id must not be the cookie value, or a database leak hands over
	// live sessions.
	if id1 == tok1 {
		t.Error("stored id equals the cookie value")
	}
	if HashToken(tok1) != id1 {
		t.Error("HashToken does not reproduce the stored id")
	}
}

// --- rate limiter ---

func TestLimiterBlocksAfterMaxAttempts(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allowed("user:stuart") {
			t.Fatalf("blocked early, after %d failures", i)
		}
		l.RecordFailure("user:stuart")
	}
	if l.Allowed("user:stuart") {
		t.Error("should be blocked after 3 failures")
	}
	if l.RetryAfter("user:stuart") <= 0 {
		t.Error("RetryAfter should be positive while blocked")
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	l.RecordFailure("user:stuart", "ip:10.0.0.1")
	l.RecordFailure("user:stuart", "ip:10.0.0.1")

	if l.Allowed("user:stuart") {
		t.Error("stuart should be blocked")
	}
	// A different account from a different address is unaffected: lockout must
	// not become a denial-of-service against everyone else.
	if !l.Allowed("user:someone-else", "ip:10.0.0.2") {
		t.Error("unrelated key should not be blocked")
	}
	// ...but the same IP is blocked regardless of which username it tries.
	if l.Allowed("user:someone-else", "ip:10.0.0.1") {
		t.Error("the offending IP should be blocked across usernames")
	}
}

func TestLimiterWindowExpires(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	base := time.Now()
	l.now = func() time.Time { return base }

	l.RecordFailure("ip:10.0.0.1")
	l.RecordFailure("ip:10.0.0.1")
	if l.Allowed("ip:10.0.0.1") {
		t.Fatal("should be blocked")
	}

	l.now = func() time.Time { return base.Add(61 * time.Second) }
	if !l.Allowed("ip:10.0.0.1") {
		t.Error("attempts should age out of the window")
	}
	if l.RetryAfter("ip:10.0.0.1") != 0 {
		t.Error("RetryAfter should be zero once unblocked")
	}
}

func TestLimiterResetOnSuccess(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	l.RecordFailure("user:stuart")
	l.RecordFailure("user:stuart")
	l.Reset("user:stuart")

	// A successful login clears the count, so two earlier typos do not leave the
	// next session one mistake from a lockout.
	for i := 0; i < 3; i++ {
		if !l.Allowed("user:stuart") {
			t.Fatalf("blocked after reset at attempt %d", i)
		}
		l.RecordFailure("user:stuart")
	}
}

func TestLimiterPrunesMap(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	base := time.Now()
	l.now = func() time.Time { return base }
	for i := 0; i < 100; i++ {
		l.RecordFailure("ip:10.0.0.1")
	}
	l.now = func() time.Time { return base.Add(2 * time.Minute) }
	l.Allowed("ip:10.0.0.1") // read prunes

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.attempts) != 0 {
		t.Errorf("expired entries left behind: %d keys", len(l.attempts))
	}
}
