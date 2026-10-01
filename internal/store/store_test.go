package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/model"
)

var ctx = context.Background()

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "sub", "pharos.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var base = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	for i := 0; i < 3; i++ {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		s.Close()
	}
}

func TestMemoryDatabase(t *testing.T) {
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnsureMonitor(ctx, "a", base); err != nil {
		t.Fatal(err)
	}
}

func TestSecretIsStable(t *testing.T) {
	s := open(t)
	a, err := s.Secret(ctx, "session")
	if err != nil || len(a) != 32 {
		t.Fatalf("secret %x %v", a, err)
	}
	b, _ := s.Secret(ctx, "session")
	c, _ := s.Secret(ctx, "other")
	if string(a) != string(b) || string(a) == string(c) {
		t.Error("secrets must be stable per key and distinct across keys")
	}
}

func TestTransitionsPeriodsAndIncidents(t *testing.T) {
	s := open(t)
	_ = s.EnsureMonitor(ctx, "web", base)

	if inc, err := s.ApplyTransition(ctx, Transition{MonitorID: "web", To: model.StatusUp, At: base}); err != nil || inc != nil {
		t.Fatalf("up: %v %v", inc, err)
	}
	inc, err := s.ApplyTransition(ctx, Transition{MonitorID: "web", To: model.StatusDown, At: base.Add(time.Hour), Cause: "HTTP 502 Bad Gateway"})
	if err != nil || inc == nil || inc.Cause != "HTTP 502 Bad Gateway" || !inc.Ongoing() {
		t.Fatalf("down: %+v %v", inc, err)
	}
	// A second down transition must not open a second incident.
	if again, err := s.ApplyTransition(ctx, Transition{MonitorID: "web", To: model.StatusDown, At: base.Add(90 * time.Minute)}); err != nil || again != nil {
		t.Fatalf("duplicate down: %+v %v", again, err)
	}
	closed, err := s.ApplyTransition(ctx, Transition{MonitorID: "web", To: model.StatusUp, At: base.Add(2 * time.Hour), Resolution: "recovered"})
	if err != nil || closed == nil || closed.ID != inc.ID || closed.Ongoing() || closed.Duration(time.Time{}) != time.Hour {
		t.Fatalf("recover: %+v %v", closed, err)
	}

	ps, err := s.Periods(ctx, base, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := ps["web"]
	if len(got) != 4 || got[1].Status != model.StatusDown || !got[3].End.IsZero() || !got[0].End.Equal(base.Add(time.Hour)) {
		t.Fatalf("periods %+v", got)
	}

	incs, err := s.Incidents(ctx, IncidentQuery{MonitorIDs: []string{"web"}})
	if err != nil || len(incs) != 1 || incs[0].Resolution != "recovered" {
		t.Fatalf("incidents %+v %v", incs, err)
	}
	if one, _ := s.Incident(ctx, inc.ID); one == nil || one.Cause != "HTTP 502 Bad Gateway" {
		t.Errorf("Incident(id) = %+v", one)
	}
	if none, _ := s.Incident(ctx, 999); none != nil {
		t.Error("missing incident should be nil")
	}
}

func TestLoadStates(t *testing.T) {
	s := open(t)
	_ = s.EnsureMonitor(ctx, "web", base)
	_ = s.EnsureMonitor(ctx, "db", base)
	_ = s.EnsureMonitor(ctx, "web", base.Add(time.Hour)) // keeps first_seen
	_ = s.SetPaused(ctx, "db", true)
	_, _ = s.ApplyTransition(ctx, Transition{MonitorID: "web", To: model.StatusDown, At: base.Add(time.Minute), Cause: "refused"})
	_ = s.InsertCheck(ctx, model.Check{MonitorID: "web", At: base.Add(2 * time.Minute), Status: model.StatusDown, Message: "refused", Latency: 3 * time.Millisecond})

	st, err := s.LoadStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	web := st["web"]
	if !web.FirstSeen.Equal(base) || web.Status != model.StatusDown || web.Incident == nil || web.LastCheck == nil || web.LastCheck.Message != "refused" {
		t.Fatalf("web state %+v", web)
	}
	if !st["db"].Paused || st["db"].Status != model.StatusUnknown {
		t.Errorf("db state %+v", st["db"])
	}
}

func TestChecksRoundTripAndFilters(t *testing.T) {
	s := open(t)
	timing := &model.Timing{DNS: 2 * time.Millisecond, Connect: 5 * time.Millisecond, TLS: 20 * time.Millisecond, FirstByte: 40 * time.Millisecond}
	for i := 0; i < 10; i++ {
		c := model.Check{MonitorID: "web", At: base.Add(time.Duration(i) * time.Minute), Status: model.StatusUp, Latency: time.Duration(100+i) * time.Millisecond, Timing: timing}
		if i%3 == 0 {
			c.Status, c.Message, c.Timing = model.StatusDown, "timed out after 10s", nil
		}
		if err := s.InsertCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.Checks(ctx, CheckQuery{MonitorID: "web", Limit: 5})
	if err != nil || len(all) != 5 || !all[0].At.Equal(base.Add(9*time.Minute)) {
		t.Fatalf("latest checks %+v %v", all, err)
	}
	if *all[1].Timing != *timing || all[1].Latency != 108*time.Millisecond {
		t.Errorf("timing round trip: %+v", all[1])
	}
	fails, _ := s.Checks(ctx, CheckQuery{MonitorID: "web", FailuresOnly: true})
	if len(fails) != 4 {
		t.Errorf("failures = %d, want 4", len(fails))
	}
	older, _ := s.Checks(ctx, CheckQuery{MonitorID: "web", Before: base.Add(2 * time.Minute)})
	if len(older) != 2 {
		t.Errorf("paging = %d, want 2", len(older))
	}
	last, _ := s.LastCheck(ctx, "web")
	if last == nil || last.Status != model.StatusDown || last.Timing != nil {
		t.Errorf("last check %+v", last)
	}
}

func TestRollupAndLatencySeries(t *testing.T) {
	s := open(t)
	// Three hours of checks every minute; hour 1 has a slow tail.
	for m := 0; m < 180; m++ {
		lat := 100 * time.Millisecond
		if m >= 60 && m < 120 && m%15 == 0 { // 4 of 60: above the 95th percentile
			lat = 900 * time.Millisecond
		}
		st := model.StatusUp
		if m == 150 {
			st = model.StatusDown
			lat = 10 * time.Second // failed checks must not skew latency
		}
		_ = s.InsertCheck(ctx, model.Check{MonitorID: "api", At: base.Add(time.Duration(m) * time.Minute), Status: st, Latency: lat})
	}
	now := base.Add(170 * time.Minute) // inside hour 2, which is not complete
	if err := s.Rollup(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx, now); err != nil { // idempotent
		t.Fatal(err)
	}
	var n int
	_ = s.r.QueryRow(`SELECT COUNT(*) FROM latency_hourly`).Scan(&n)
	if n != 2 {
		t.Fatalf("rolled up %d hours, want 2 complete hours", n)
	}

	hourly, err := s.LatencySeries(ctx, "api", base, base.Add(3*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 3 {
		t.Fatalf("hourly points = %d, want 3 (2 rolled up + 1 from raw)", len(hourly))
	}
	if hourly[0].Avg != 100*time.Millisecond || hourly[0].Checks != 60 {
		t.Errorf("hour 0 %+v", hourly[0])
	}
	if hourly[1].Max != 900*time.Millisecond || hourly[1].P95 != 900*time.Millisecond {
		t.Errorf("hour 1 %+v", hourly[1])
	}
	if hourly[2].Checks != 60 || hourly[2].OK != 59 || hourly[2].Max != 100*time.Millisecond {
		t.Errorf("hour 2 (raw) %+v", hourly[2])
	}

	three, _ := s.LatencySeries(ctx, "api", base, base.Add(3*time.Hour), 3*time.Hour)
	if len(three) != 1 || three[0].Checks != 180 || three[0].OK != 179 {
		t.Errorf("3h bucket %+v", three)
	}

	fine, _ := s.LatencySeries(ctx, "api", base, base.Add(time.Hour), 10*time.Minute)
	if len(fine) != 6 || fine[0].Checks != 10 {
		t.Errorf("10m buckets %+v", fine)
	}
}

func TestLatencySeriesAllMatchesPerMonitor(t *testing.T) {
	s := open(t)
	for i, id := range []string{"api", "db", "web"} {
		if err := s.EnsureMonitor(ctx, id, base); err != nil {
			t.Fatal(err)
		}
		// Three hours of checks, one monitor with failures, one with a
		// maintenance window whose checks must not count.
		for m := 0; m < 180; m++ {
			c := model.Check{MonitorID: id, At: base.Add(time.Duration(m)*time.Minute + time.Duration(i)*time.Second),
				Status: model.StatusUp, Latency: time.Duration(50+10*i+m%7) * time.Millisecond}
			if id == "db" && m%40 == 0 {
				c.Status, c.Latency = model.StatusDown, 5*time.Second
			}
			if id == "web" && m >= 100 && m < 110 {
				c.Maintenance, c.Latency = true, 3*time.Second
			}
			if err := s.InsertCheck(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Rollup(ctx, base.Add(170*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		from, to := base.Add(30*time.Minute), base.Add(175*time.Minute)
		all, err := s.LatencySeriesAll(ctx, from, to, bucket)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 3 {
			t.Fatalf("%s: %d monitors, want 3", bucket, len(all))
		}
		for _, id := range []string{"api", "db", "web"} {
			one, err := s.LatencySeries(ctx, id, from, to, bucket)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(one) != fmt.Sprint(all[id]) {
				t.Errorf("%s %s:\n one: %v\n all: %v", bucket, id, one, all[id])
			}
		}
	}
	if _, err := s.LatencySeriesAll(ctx, base, base.Add(time.Hour), 10*time.Minute); err == nil {
		t.Error("buckets under an hour: want an error")
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	for d := 0; d < 40; d++ {
		_ = s.InsertCheck(ctx, model.Check{MonitorID: "web", At: base.Add(time.Duration(d) * 24 * time.Hour), Status: model.StatusUp})
	}
	now := base.Add(40 * 24 * time.Hour)
	n, err := s.Prune(ctx, now, 30*24*time.Hour, 400*24*time.Hour)
	if err != nil || n != 10 {
		t.Fatalf("pruned %d %v, want 10", n, err)
	}
	left, _ := s.Checks(ctx, CheckQuery{MonitorID: "web", Limit: 1000})
	if len(left) != 30 {
		t.Errorf("%d checks left", len(left))
	}
}

func TestNotificationLog(t *testing.T) {
	s := open(t)
	id, err := s.AddNotification(ctx, NotificationLog{Created: base, MonitorID: "web", IncidentID: 3, Event: "down", Notifier: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := s.AddNotification(ctx, NotificationLog{Created: base, Event: "up", Notifier: "ops"})
	_ = s.UpdateNotification(ctx, id, NotifyDelivered, 2, "", base.Add(time.Second))
	_ = s.FailStaleNotifications(ctx)
	log, err := s.Notifications(ctx, 10)
	if err != nil || len(log) != 2 {
		t.Fatalf("log %+v %v", log, err)
	}
	if log[0].ID != id2 || log[0].State != NotifyFailed || log[0].LastError == "" {
		t.Errorf("stale entry %+v", log[0])
	}
	if log[1].State != NotifyDelivered || log[1].Attempts != 2 || log[1].Delivered.IsZero() {
		t.Errorf("delivered entry %+v", log[1])
	}
}

func TestPercentile(t *testing.T) {
	v := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if percentile(v, 0.5) != 5 || percentile(v, 0.95) != 10 || percentile(v[:1], 0.95) != 1 || percentile(nil, 0.5) != 0 {
		t.Error("nearest-rank percentile")
	}
}

// A reader holding a snapshot, such as a backup tool, must not hold up the
// checkpoint or the writes that follow it.
func TestCheckpointDoesNotWaitForReaders(t *testing.T) {
	s := open(t)
	for i := range 2000 {
		_ = s.InsertCheck(ctx, model.Check{MonitorID: "api", At: base.Add(time.Duration(i) * time.Second), Status: model.StatusUp})
	}
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM checks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCheck(ctx, model.Check{MonitorID: "api", At: base, Status: model.StatusUp}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("checkpoint and write took %s with a reader open", d)
	}
	var limit int64
	if err := s.w.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil || limit != 64<<20 {
		t.Errorf("journal_size_limit = %d (%v), want 64 MiB", limit, err)
	}
}
