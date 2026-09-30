package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/useless-husband/pharos/internal/model"
)

// InsertCheck stores one check result.
func (s *Store) InsertCheck(ctx context.Context, c model.Check) error {
	var t model.Timing
	hasTiming := c.Timing != nil
	if hasTiming {
		t = *c.Timing
	}
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO checks(monitor_id, at, status, latency_us, message, dns_us, connect_us, tls_us, ttfb_us, has_timing, cert_expiry, maintenance)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.MonitorID, ms(c.At), int(c.Status), c.Latency.Microseconds(), c.Message,
		t.DNS.Microseconds(), t.Connect.Microseconds(), t.TLS.Microseconds(), t.FirstByte.Microseconds(),
		hasTiming, ms(c.CertExpiry), c.Maintenance)
	return err
}

// InsertChecks stores many results in one transaction.
func (s *Store) InsertChecks(ctx context.Context, cs []model.Check) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO checks(monitor_id, at, status, latency_us, message, dns_us, connect_us, tls_us, ttfb_us, has_timing, cert_expiry, maintenance)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range cs {
			var t model.Timing
			if c.Timing != nil {
				t = *c.Timing
			}
			if _, err := stmt.ExecContext(ctx, c.MonitorID, ms(c.At), int(c.Status), c.Latency.Microseconds(), c.Message,
				t.DNS.Microseconds(), t.Connect.Microseconds(), t.TLS.Microseconds(), t.FirstByte.Microseconds(),
				c.Timing != nil, ms(c.CertExpiry), c.Maintenance); err != nil {
				return err
			}
		}
		return nil
	})
}

const checkCols = `monitor_id, at, status, latency_us, message, dns_us, connect_us, tls_us, ttfb_us, has_timing, cert_expiry, maintenance`

func scanCheck(sc interface{ Scan(...any) error }) (model.Check, error) {
	var c model.Check
	var at, lat, dns, conn, tlsUS, ttfb, cert int64
	var status int
	var hasTiming bool
	err := sc.Scan(&c.MonitorID, &at, &status, &lat, &c.Message, &dns, &conn, &tlsUS, &ttfb, &hasTiming, &cert, &c.Maintenance)
	if err != nil {
		return c, err
	}
	c.At, c.Status, c.Latency, c.CertExpiry = fromMS(at), model.Status(status), time.Duration(lat)*time.Microsecond, fromMS(cert)
	if hasTiming {
		c.Timing = &model.Timing{
			DNS:       time.Duration(dns) * time.Microsecond,
			Connect:   time.Duration(conn) * time.Microsecond,
			TLS:       time.Duration(tlsUS) * time.Microsecond,
			FirstByte: time.Duration(ttfb) * time.Microsecond,
		}
	}
	return c, nil
}

// LastCheck returns the most recent check of a monitor, or nil.
func (s *Store) LastCheck(ctx context.Context, id string) (*model.Check, error) {
	row := s.w.QueryRowContext(ctx, `SELECT `+checkCols+` FROM checks WHERE monitor_id = ? ORDER BY at DESC LIMIT 1`, id)
	c, err := scanCheck(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CountChecks returns the number of check results stored since t.
func (s *Store) CountChecks(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM checks WHERE at >= ?`, ms(since)).Scan(&n)
	return n, err
}

// CheckQuery filters Checks.
type CheckQuery struct {
	MonitorID    string
	Before       time.Time // exclusive; zero means now
	Limit        int
	FailuresOnly bool
}

// Checks returns checks newest first.
func (s *Store) Checks(ctx context.Context, q CheckQuery) ([]model.Check, error) {
	query := `SELECT ` + checkCols + ` FROM checks WHERE monitor_id = ?`
	args := []any{q.MonitorID}
	if !q.Before.IsZero() {
		query += ` AND at < ?`
		args = append(args, ms(q.Before))
	}
	if q.FailuresOnly {
		query += ` AND status != ?`
		args = append(args, int(model.StatusUp))
	}
	query += ` ORDER BY at DESC`
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Prune deletes check results older than retention, and history older than
// historyKeep. It deletes in batches so the writer is never held for long.
func (s *Store) Prune(ctx context.Context, now time.Time, retention, historyKeep time.Duration) (int64, error) {
	cutoff := ms(now.Add(-retention))
	var total int64
	for {
		res, err := s.w.ExecContext(ctx, `DELETE FROM checks WHERE rowid IN (SELECT rowid FROM checks WHERE at < ? LIMIT 5000)`, cutoff)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 || ctx.Err() != nil {
			break
		}
	}
	old := ms(now.Add(-historyKeep))
	for _, q := range []string{
		`DELETE FROM latency_hourly WHERE hour < ?`,
		`DELETE FROM periods WHERE ended IS NOT NULL AND ended < ?`,
		`DELETE FROM incidents WHERE ended IS NOT NULL AND ended < ?`,
		`DELETE FROM notifications WHERE created < ?`,
	} {
		if _, err := s.w.ExecContext(ctx, q, old); err != nil {
			return total, err
		}
	}
	return total, nil
}

// LatencyPoint aggregates the successful checks in one time bucket.
type LatencyPoint struct {
	Start  time.Time
	Checks int // all checks in the bucket
	OK     int // checks that were up or degraded
	Avg    time.Duration
	P95    time.Duration
	Max    time.Duration
}

// rollupLag is how long after an hour ends it is aggregated.
const rollupLag = 15 * time.Minute

// Rollup aggregates completed hours of raw checks into latency_hourly.
// It is idempotent and resumes from where the last run stopped.
func (s *Store) Rollup(ctx context.Context, now time.Time) error {
	var until int64
	err := s.w.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'rollup_until'`).Scan(&until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Only roll up hours that ended a while ago: a check that started just
	// before the hour may still be running when the hour ends.
	current := now.Add(-rollupLag).Truncate(time.Hour)
	if until == 0 {
		var first sql.NullInt64
		if err := s.w.QueryRowContext(ctx, `SELECT MIN(at) FROM checks`).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		until = ms(fromMS(first.Int64).Truncate(time.Hour))
	}
	for h := fromMS(until); h.Before(current); h = h.Add(time.Hour) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.rollupHour(ctx, h); err != nil {
			return err
		}
		if _, err := s.w.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('rollup_until', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, ms(h.Add(time.Hour))); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) rollupHour(ctx context.Context, h time.Time) error {
	rows, err := s.w.QueryContext(ctx, `SELECT monitor_id, status, latency_us FROM checks WHERE at >= ? AND at < ? AND maintenance = 0`, ms(h), ms(h.Add(time.Hour)))
	if err != nil {
		return err
	}
	type agg struct {
		checks int
		lats   []int64
	}
	byMon := map[string]*agg{}
	for rows.Next() {
		var id string
		var status int
		var lat int64
		if err := rows.Scan(&id, &status, &lat); err != nil {
			rows.Close()
			return err
		}
		a := byMon[id]
		if a == nil {
			a = &agg{}
			byMon[id] = a
		}
		a.checks++
		if model.Status(status).Available() {
			a.lats = append(a.lats, lat)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for id, a := range byMon {
			var sum, mn, mx, p50, p95 int64
			if len(a.lats) > 0 {
				sort.Slice(a.lats, func(i, j int) bool { return a.lats[i] < a.lats[j] })
				for _, v := range a.lats {
					sum += v
				}
				mn, mx = a.lats[0], a.lats[len(a.lats)-1]
				p50, p95 = percentile(a.lats, 0.50), percentile(a.lats, 0.95)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO latency_hourly(monitor_id, hour, checks, ok, sum_us, min_us, max_us, p50_us, p95_us)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(monitor_id, hour) DO UPDATE SET checks = excluded.checks, ok = excluded.ok,
					sum_us = excluded.sum_us, min_us = excluded.min_us, max_us = excluded.max_us,
					p50_us = excluded.p50_us, p95_us = excluded.p95_us`,
				id, ms(h), a.checks, len(a.lats), sum, mn, mx, p50, p95); err != nil {
				return err
			}
		}
		return nil
	})
}

// percentile uses the nearest-rank method on sorted values.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p*float64(len(sorted))+0.999999) - 1
	rank = max(0, min(rank, len(sorted)-1))
	return sorted[rank]
}

// LatencySeries returns latency per bucket for [from, to). Buckets finer
// than an hour are computed from raw checks. Coarser buckets combine hourly
// aggregates; hours that have not been rolled up yet (the current one, or
// all of them before the first rollup) are aggregated from raw checks on
// the fly. The P95 of a multi-hour bucket is the largest hourly P95, a
// conservative bound. Buckets are aligned to from + k*bucket.
func (s *Store) LatencySeries(ctx context.Context, id string, from, to time.Time, bucket time.Duration) ([]LatencyPoint, error) {
	if bucket < time.Hour {
		return s.rawSeries(ctx, id, from, to, bucket)
	}
	hours, err := s.hourly(ctx, id, from, to)
	if err != nil {
		return nil, err
	}
	return bucketize(hours[id], from, to, bucket), nil
}

// LatencySeriesAll is LatencySeries for every monitor at once, keyed by
// monitor id, for buckets of an hour or more. Pages that list every monitor
// use it: it reads the database in a few passes instead of a few queries
// per monitor.
func (s *Store) LatencySeriesAll(ctx context.Context, from, to time.Time, bucket time.Duration) (map[string][]LatencyPoint, error) {
	if bucket < time.Hour {
		return nil, fmt.Errorf("LatencySeriesAll: bucket %s is shorter than an hour", bucket)
	}
	hours, err := s.hourly(ctx, "", from, to)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]LatencyPoint, len(hours))
	for id, hs := range hours {
		out[id] = bucketize(hs, from, to, bucket)
	}
	return out, nil
}

type hourAgg struct {
	checks, ok    int
	sum, max, p95 int64
}

// hourly returns hourly aggregates in [from, to) keyed by monitor id and
// hour: rolled-up hours from latency_hourly, the others from raw checks.
// An empty id means every monitor.
func (s *Store) hourly(ctx context.Context, id string, from, to time.Time) (map[string]map[int64]hourAgg, error) {
	out := map[string]map[int64]hourAgg{}
	put := func(mon string, h int64, a hourAgg) {
		m := out[mon]
		if m == nil {
			m = map[int64]hourAgg{}
			out[mon] = m
		}
		if _, done := m[h]; !done {
			m[h] = a
		}
	}
	q := `SELECT monitor_id, hour, checks, ok, sum_us, max_us, p95_us FROM latency_hourly WHERE monitor_id = ? AND hour >= ? AND hour < ?`
	args := []any{id, ms(from.Truncate(time.Hour)), ms(to)}
	if id == "" {
		// Seek the primary key once per monitor rather than scanning every
		// monitor's 400 days of aggregates.
		q = `SELECT monitor_id, hour, checks, ok, sum_us, max_us, p95_us FROM latency_hourly WHERE monitor_id IN (SELECT id FROM monitors) AND hour >= ? AND hour < ?`
		args = args[1:]
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var mon string
		var h int64
		var a hourAgg
		if err := rows.Scan(&mon, &h, &a.checks, &a.ok, &a.sum, &a.max, &a.p95); err != nil {
			rows.Close()
			return nil, err
		}
		put(mon, h, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hours missing from the rollup table come from raw checks, limited to
	// the raw data that can exist (retention) by the query itself.
	var rolledUntil sql.NullInt64
	_ = s.r.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'rollup_until'`).Scan(&rolledUntil)
	rawFrom := from
	if rolledUntil.Valid && fromMS(rolledUntil.Int64).After(rawFrom) {
		rawFrom = fromMS(rolledUntil.Int64)
	}
	if !rawFrom.Before(to) {
		return out, nil
	}
	q = `SELECT monitor_id, at, status, latency_us FROM checks WHERE monitor_id = ? AND at >= ? AND at < ? AND maintenance = 0`
	args = []any{id, ms(rawFrom.Truncate(time.Hour)), ms(to)}
	if id == "" {
		q = `SELECT monitor_id, at, status, latency_us FROM checks WHERE at >= ? AND at < ? AND maintenance = 0`
		args = args[1:]
	}
	rows, err = s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	type key struct {
		mon  string
		hour int64
	}
	raw := map[key]*struct {
		checks int
		lats   []int64
	}{}
	for rows.Next() {
		var mon string
		var at, lat int64
		var status int
		if err := rows.Scan(&mon, &at, &status, &lat); err != nil {
			rows.Close()
			return nil, err
		}
		k := key{mon, ms(fromMS(at).Truncate(time.Hour))}
		a := raw[k]
		if a == nil {
			a = &struct {
				checks int
				lats   []int64
			}{}
			raw[k] = a
		}
		a.checks++
		if model.Status(status).Available() {
			a.lats = append(a.lats, lat)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, a := range raw {
		h := hourAgg{checks: a.checks, ok: len(a.lats)}
		if len(a.lats) > 0 {
			sort.Slice(a.lats, func(i, j int) bool { return a.lats[i] < a.lats[j] })
			for _, v := range a.lats {
				h.sum += v
			}
			h.max, h.p95 = a.lats[len(a.lats)-1], percentile(a.lats, 0.95)
		}
		put(k.mon, k.hour, h)
	}
	return out, nil
}

// bucketize combines hourly aggregates into buckets aligned to from.
func bucketize(hours map[int64]hourAgg, from, to time.Time, bucket time.Duration) []LatencyPoint {
	type acc struct {
		LatencyPoint
		sum int64
	}
	buckets := map[int64]*acc{}
	for h, a := range hours {
		ht := fromMS(h)
		if ht.Before(from.Truncate(time.Hour)) || !ht.Before(to) {
			continue
		}
		k := ms(bucketStart(ht, from, bucket))
		b := buckets[k]
		if b == nil {
			b = &acc{LatencyPoint: LatencyPoint{Start: fromMS(k)}}
			buckets[k] = b
		}
		b.Checks += a.checks
		b.OK += a.ok
		b.sum += a.sum
		b.Max = max(b.Max, time.Duration(a.max)*time.Microsecond)
		b.P95 = max(b.P95, time.Duration(a.p95)*time.Microsecond)
	}
	out := make([]LatencyPoint, 0, len(buckets))
	for _, b := range buckets {
		if b.OK > 0 {
			b.Avg = time.Duration(b.sum/int64(b.OK)) * time.Microsecond
		}
		out = append(out, b.LatencyPoint)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func bucketStart(t, origin time.Time, bucket time.Duration) time.Time {
	if t.Before(origin) {
		return origin
	}
	return origin.Add(t.Sub(origin) / bucket * bucket)
}

func (s *Store) rawSeries(ctx context.Context, id string, from, to time.Time, bucket time.Duration) ([]LatencyPoint, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT at, status, latency_us FROM checks WHERE monitor_id = ? AND at >= ? AND at < ? AND maintenance = 0 ORDER BY at`, id, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LatencyPoint
	var lats []int64
	var cur *LatencyPoint
	flush := func() {
		if cur == nil {
			return
		}
		if len(lats) > 0 {
			sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
			var sum int64
			for _, v := range lats {
				sum += v
			}
			cur.Avg = time.Duration(sum/int64(len(lats))) * time.Microsecond
			cur.P95 = time.Duration(percentile(lats, 0.95)) * time.Microsecond
			cur.Max = time.Duration(lats[len(lats)-1]) * time.Microsecond
		}
		out = append(out, *cur)
		lats = lats[:0]
	}
	for rows.Next() {
		var at, lat int64
		var status int
		if err := rows.Scan(&at, &status, &lat); err != nil {
			return nil, err
		}
		start := bucketStart(fromMS(at), from, bucket)
		if cur == nil || !cur.Start.Equal(start) {
			flush()
			cur = &LatencyPoint{Start: start}
		}
		cur.Checks++
		if model.Status(status).Available() {
			cur.OK++
			lats = append(lats, lat)
		}
	}
	flush()
	return out, rows.Err()
}
