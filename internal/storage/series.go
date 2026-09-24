package storage

import (
	"database/sql"
	"math"
	"time"
)

// Series bucket bounds. A chart has a few hundred pixels of width; asking for
// more points than that returns data no one can see and makes SQLite sort rows
// for nothing.
const (
	DefaultSeriesBuckets = 96
	MaxSeriesBuckets     = 1000
)

// SeriesPoint is one bucket of the time series. Buckets with no checks are
// still emitted, with Checks zero, so the client gets a dense array and a gap in
// monitoring is visible rather than silently closed up.
type SeriesPoint struct {
	TS            int64   `json:"ts"`
	Checks        int64   `json:"checks"`
	Successful    int64   `json:"successful"`
	UptimePercent float64 `json:"uptime_percent"`
	AvgMs         float64 `json:"avg_ms"`
	P95Ms         int64   `json:"p95_ms"`
}

// Series is bucketed uptime and latency over a window.
type Series struct {
	Window        string        `json:"window"`
	From          int64         `json:"from"`
	To            int64         `json:"to"`
	BucketSeconds int64         `json:"bucket_seconds"`
	Points        []SeriesPoint `json:"points"`
}

// Series aggregates check results into at most `buckets` evenly spaced points.
//
// This exists because charting from /results does not scale: at a 60-second
// interval a 30-day window is roughly 43,000 rows per site, all of which would
// cross the wire to be averaged into a few hundred pixels. Bucketing in SQL
// sends back only what is drawn.
func (s *SiteStore) Series(window string, sinceTS int64, buckets int) (Series, error) {
	if buckets <= 0 {
		buckets = DefaultSeriesBuckets
	}
	if buckets > MaxSeriesBuckets {
		buckets = MaxSeriesBuckets
	}

	out := Series{Window: window, To: time.Now().Unix(), Points: []SeriesPoint{}}

	from := sinceTS
	if from <= 0 {
		// "all": start at the first recorded check. No rows means an empty
		// series rather than a window stretching back to the epoch.
		var first sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(ts) FROM check_results`).Scan(&first); err != nil {
			return out, err
		}
		if !first.Valid {
			out.From, out.BucketSeconds = out.To, 1
			return out, nil
		}
		from = first.Int64
	}
	if from > out.To {
		from = out.To
	}
	out.From = from

	span := out.To - from
	if span < 1 {
		span = 1
	}
	bucketSecs := (span + int64(buckets) - 1) / int64(buckets) // ceil
	if bucketSecs < 1 {
		bucketSecs = 1
	}
	out.BucketSeconds = bucketSecs

	// Pre-fill every bucket so gaps in monitoring stay visible as zero-check
	// points instead of vanishing from the array.
	points := make([]SeriesPoint, buckets)
	for i := range points {
		points[i] = SeriesPoint{TS: from + int64(i)*bucketSecs}
	}

	// Counts and the mean, in one grouped pass. The mean covers successful
	// checks only, matching Metrics and the percentile below.
	//
	// The bucket index is clamped in SQL, not afterwards in Go: a tick landing
	// exactly on the window's upper edge divides to one slot past the end, and
	// folding it in here means SQLite aggregates it with the final bucket.
	// Clamping after the fact would instead produce two result rows sharing an
	// index, the second overwriting the first.
	lastIdx := int64(buckets - 1)
	rows, err := s.db.Query(`SELECT MIN((ts - ?) / ?, ?) AS b,
			COUNT(*), COALESCE(SUM(up), 0),
			AVG(CASE WHEN up = 1 THEN response_ms END)
		FROM check_results WHERE ts >= ? AND ts <= ?
		GROUP BY b ORDER BY b`, from, bucketSecs, lastIdx, from, out.To)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			b     int64
			n     int64
			ok    int64
			avgMs sql.NullFloat64
		)
		if err := rows.Scan(&b, &n, &ok, &avgMs); err != nil {
			return out, err
		}
		if b < 0 || b >= int64(len(points)) {
			continue // defensive: the query clamps, so this cannot normally fire
		}
		p := &points[b]
		p.Checks, p.Successful = n, ok
		if n > 0 {
			p.UptimePercent = math.Round(float64(ok)/float64(n)*10000) / 100
		}
		p.AvgMs = math.Round(avgMs.Float64*100) / 100
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	if err := s.fillSeriesP95(points, from, bucketSecs, lastIdx, out.To); err != nil {
		return out, err
	}
	out.Points = points
	return out, nil
}

// fillSeriesP95 adds the 95th-percentile latency to each bucket.
//
// Kept as a second query rather than folded into the first: mixing per-row
// window functions with GROUP BY aggregates in one statement is possible but
// unreadable, and both passes hit the same index-free table scan either way.
func (s *SiteStore) fillSeriesP95(points []SeriesPoint, from, bucketSecs, lastIdx, to int64) error {
	// ROW_NUMBER ranks each bucket's latencies ascending; cut is the 95th
	// percentile position (at least 1). Taking MAX over the rows at or below
	// that position yields the value at it. The partition key is clamped the
	// same way as the counts query, so both agree on which bucket a row is in.
	rows, err := s.db.Query(`SELECT b, MAX(CASE WHEN rn <= cut THEN response_ms END)
		FROM (
			SELECT MIN((ts - ?) / ?, ?) AS b, response_ms,
				ROW_NUMBER() OVER (PARTITION BY MIN((ts - ?) / ?, ?) ORDER BY response_ms) AS rn,
				MAX(1, CAST(COUNT(*) OVER (PARTITION BY MIN((ts - ?) / ?, ?)) * 95 / 100 AS INTEGER)) AS cut
			FROM check_results
			WHERE ts >= ? AND ts <= ? AND up = 1 AND response_ms IS NOT NULL
		)
		GROUP BY b ORDER BY b`,
		from, bucketSecs, lastIdx, from, bucketSecs, lastIdx, from, bucketSecs, lastIdx, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			b   int64
			p95 sql.NullInt64
		)
		if err := rows.Scan(&b, &p95); err != nil {
			return err
		}
		if b >= 0 && b < int64(len(points)) {
			points[b].P95Ms = p95.Int64
		}
	}
	return rows.Err()
}
