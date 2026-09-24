package storage

import (
	"testing"
	"time"
)

// seed writes n ticks ending at `end`, one every `step` seconds, failing every
// failEvery-th tick (0 = never fail).
func seedTicks(t *testing.T, s *SiteStore, end int64, step int64, n int, failEvery int, ms int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		ts := end - int64(n-1-i)*step
		tk := Tick{TS: ts, Up: true, StatusCode: 200, ResponseMs: ms + int64(i)}
		if failEvery > 0 && i%failEvery == 0 {
			tk = Tick{TS: ts, StatusCode: 500, ResponseMs: 5}
			tk.AddFail("http", "unexpected status 500")
		}
		if _, err := s.RecordTick(tk); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeriesBucketsAndDensity(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	// 120 ticks, one per minute, over the last two hours.
	seedTicks(t, s, now, 60, 120, 0, 100)

	ser, err := s.Series("24h", now-86400, 24)
	if err != nil {
		t.Fatal(err)
	}
	// The array is dense: 24 points whether or not each has data, so a gap in
	// monitoring is visible rather than closed up.
	if len(ser.Points) != 24 {
		t.Fatalf("got %d points, want 24", len(ser.Points))
	}
	if ser.BucketSeconds != 3600 {
		t.Errorf("bucket_seconds = %d, want 3600", ser.BucketSeconds)
	}

	var withData, total int64
	for _, p := range ser.Points {
		if p.Checks > 0 {
			withData++
		}
		total += p.Checks
	}
	if total != 120 {
		t.Errorf("checks across buckets = %d, want 120", total)
	}
	// Two hours of data in a 24-hour window: most buckets are empty.
	if withData > 4 {
		t.Errorf("%d buckets have data, want at most 4", withData)
	}
}

func TestSeriesUptimeAndLatency(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	// 100 ticks over ~100s, every 4th failing => 75% uptime.
	seedTicks(t, s, now, 1, 100, 4, 100)

	ser, err := s.Series("1h", now-3600, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ser.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(ser.Points))
	}
	p := ser.Points[0]
	if p.Checks != 100 {
		t.Errorf("checks = %d, want 100", p.Checks)
	}
	if p.UptimePercent != 75 {
		t.Errorf("uptime = %v, want 75", p.UptimePercent)
	}
	// Latency covers successful checks only; the failures recorded 5ms and must
	// not drag the average down.
	if p.AvgMs < 100 {
		t.Errorf("avg_ms = %v, want >= 100 (failed checks must be excluded)", p.AvgMs)
	}
	if p.P95Ms <= 0 {
		t.Errorf("p95_ms = %d, want > 0", p.P95Ms)
	}
	if p.P95Ms < int64(p.AvgMs) {
		t.Errorf("p95 (%d) below the mean (%v)", p.P95Ms, p.AvgMs)
	}
}

// TestSeriesP95IsAPercentileNotAMax pins the distinction: one huge outlier must
// move the max but not the 95th percentile.
func TestSeriesP95IsAPercentileNotAMax(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	for i := 0; i < 100; i++ {
		if _, err := s.RecordTick(Tick{
			TS: now - int64(100-i), Up: true, StatusCode: 200, ResponseMs: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One 10-second response.
	if _, err := s.RecordTick(Tick{TS: now, Up: true, StatusCode: 200, ResponseMs: 10000}); err != nil {
		t.Fatal(err)
	}
	ser, err := s.Series("1h", now-3600, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ser.Points[0].P95Ms; got != 100 {
		t.Errorf("p95 = %d, want 100 — a single outlier must not define it", got)
	}
}

func TestSeriesEmptyStore(t *testing.T) {
	s := newStore(t)
	ser, err := s.Series("all", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ser.Points) != 0 {
		t.Errorf("got %d points for an empty store, want 0", len(ser.Points))
	}
	if ser.BucketSeconds < 1 {
		t.Errorf("bucket_seconds = %d, want >= 1", ser.BucketSeconds)
	}
}

func TestSeriesAllWindowStartsAtFirstCheck(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	first := now - 600
	seedTicks(t, s, now, 60, 11, 0, 100)

	ser, err := s.Series("all", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	// "all" must not stretch back to the epoch, or every point but the last is
	// an empty bucket.
	if ser.From != first {
		t.Errorf("from = %d, want %d (first recorded check)", ser.From, first)
	}
}

func TestSeriesBucketsClamped(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	seedTicks(t, s, now, 60, 10, 0, 100)

	ser, err := s.Series("24h", now-86400, 99999)
	if err != nil {
		t.Fatal(err)
	}
	if len(ser.Points) != MaxSeriesBuckets {
		t.Errorf("got %d points, want the %d cap", len(ser.Points), MaxSeriesBuckets)
	}
}
