package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/clock"
	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
)

func chatConfig(monitors int) *config.Config {
	cfg := &config.Config{
		StatusPage: config.StatusPage{Language: "en"},
		Server:     config.Server{BaseURL: "https://status.example.com"},
		Notifiers:  []config.Notifier{{Name: "chat", Type: "discord", URL: "http://x"}},
		Defaults:   config.Defaults{Notify: []string{"chat"}},
	}
	for i := range monitors {
		cfg.Monitors = append(cfg.Monitors, config.Monitor{ID: fmt.Sprintf("m%d", i)})
	}
	return cfg
}

func downFor(id string) model.Event {
	ev := downEvent()
	ev.Monitor.ID, ev.Monitor.Name = id, id
	ev.Incident = &model.Incident{ID: 1, MonitorID: id, Started: at}
	return ev
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func (f *fakeSender) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d messages sent, want %d", f.count(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

type groupHarness struct {
	d     *Dispatcher
	s     *fakeSender
	clock *clock.Fake
	store *store.Store
}

func newGroupHarness(t *testing.T, cfg *config.Config, s *fakeSender) *groupHarness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fc := clock.NewFake(at)
	d, err := NewDispatcher(cfg, Options{Log: st, Clock: fc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewSender: func(config.Notifier) (Sender, error) { return s, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return &groupHarness{d: d, s: s, clock: fc, store: st}
}

// A burst of alerts becomes two messages: the first alert at once, the
// rest together one group interval later.
func TestGroupingSendsFirstAtOnceAndTheRestTogether(t *testing.T) {
	h := newGroupHarness(t, chatConfig(40), &fakeSender{})
	h.d.Notify(downFor("m0"))
	h.s.waitFor(t, 1)
	for i := 1; i < 31; i++ {
		h.d.Notify(downFor(fmt.Sprintf("m%d", i)))
	}
	if !h.clock.BlockUntil(1, 5*time.Second) {
		t.Fatal("group is not waiting for its interval")
	}
	h.clock.Advance(9 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if n := h.s.count(); n != 1 {
		t.Fatalf("%d messages before the group interval ended, want 1", n)
	}
	h.clock.Advance(time.Second)
	h.s.waitFor(t, 2)
	h.d.Close(context.Background())

	first, grouped := h.s.msgs[0], h.s.msgs[1]
	if first.Count != 1 || first.Title != "DOWN: m0" {
		t.Errorf("first message %q (count %d), want the single alert", first.Title, first.Count)
	}
	if grouped.Count != 30 || grouped.Title != "Monitors: 30 down" || grouped.Severity != SeverityCritical {
		t.Errorf("grouped message %q count %d severity %d", grouped.Title, grouped.Count, grouped.Severity)
	}
	lines := strings.Split(grouped.Body, "\n")
	if len(lines) != maxGroupLines+1 || lines[0] != "• DOWN: m1 · HTTP 503 Service Unavailable" || lines[maxGroupLines] != "…and 10 more" {
		t.Errorf("grouped body:\n%s", grouped.Body)
	}
	if grouped.Link != "https://status.example.com/admin" {
		t.Errorf("grouped link %q, want the overview", grouped.Link)
	}
	// The delivery log keeps one entry per monitor.
	log, _ := h.store.Notifications(context.Background(), 100)
	if len(log) != 31 {
		t.Fatalf("%d log entries, want 31", len(log))
	}
	for _, l := range log {
		if l.State != store.NotifyDelivered || l.IncidentID != 1 {
			t.Fatalf("log entry %+v", l)
		}
	}
}

// Once a group interval has passed without a message, the next alert goes
// out at once again.
func TestGroupingDoesNotDelayIsolatedAlerts(t *testing.T) {
	h := newGroupHarness(t, chatConfig(3), &fakeSender{})
	h.d.Notify(downFor("m0"))
	h.s.waitFor(t, 1)
	h.clock.Advance(11 * time.Second)
	h.d.Notify(downFor("m1"))
	h.s.waitFor(t, 2) // no clock advance needed
	h.d.Close(context.Background())
}

func TestGroupIntervalZeroSendsEachAlert(t *testing.T) {
	cfg := chatConfig(3)
	zero := config.Duration(0)
	cfg.Notifiers[0].GroupInterval = &zero
	h := newGroupHarness(t, cfg, &fakeSender{})
	for i := range 3 {
		h.d.Notify(downFor(fmt.Sprintf("m%d", i)))
	}
	h.s.waitFor(t, 3)
	h.d.Close(context.Background())
}

// Close sends what is waiting for its group interval instead of dropping it.
func TestCloseFlushesPendingGroups(t *testing.T) {
	h := newGroupHarness(t, chatConfig(3), &fakeSender{})
	h.d.Notify(downFor("m0"))
	h.s.waitFor(t, 1)
	h.d.Notify(downFor("m1"))
	h.d.Notify(downFor("m2"))
	h.clock.BlockUntil(1, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.d.Close(ctx)
	if n := h.s.count(); n != 2 || h.s.msgs[1].Count != 2 {
		t.Fatalf("%d messages after Close, want the pending two as one", n)
	}
}

// A 429 waits as long as the service asked, and does not use up retries.
func TestRateLimitWaitsAsAskedWithoutUsingRetries(t *testing.T) {
	limit := &RateLimitError{After: 7 * time.Second, Err: errors.New("HTTP 429")}
	s := &fakeSender{errs: []error{limit, limit, limit}}
	cfg := chatConfig(1)
	zero := config.Duration(0)
	cfg.Notifiers[0].GroupInterval = &zero
	h := newGroupHarness(t, cfg, s)
	h.d.backoff = []time.Duration{time.Millisecond} // one retry for real failures
	h.d.Notify(downFor("m0"))
	for want := 2; want <= 4; want++ {
		if !h.clock.BlockUntil(1, 5*time.Second) {
			t.Fatal("not waiting after a 429")
		}
		h.clock.Advance(6 * time.Second)
		time.Sleep(20 * time.Millisecond)
		if n := int(atomic.LoadInt32(&s.calls)); n != want-1 {
			t.Fatalf("sent again before Retry-After passed: %d calls", n)
		}
		h.clock.Advance(time.Second)
		for int(atomic.LoadInt32(&s.calls)) < want {
			time.Sleep(time.Millisecond)
		}
	}
	h.d.Close(context.Background())
	log, _ := h.store.Notifications(context.Background(), 10)
	if len(log) != 1 || log[0].State != store.NotifyDelivered || log[0].Attempts != 4 {
		t.Fatalf("log %+v, want delivered after 4 attempts", log)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	header := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	for _, tc := range []struct {
		name string
		h    http.Header
		body string
		want time.Duration
	}{
		{"seconds", header("3"), "", 3 * time.Second},
		{"fractional", header("1.5"), "", 1500 * time.Millisecond},
		{"http date", header(now.Add(90 * time.Second).Format(http.TimeFormat)), "", 90 * time.Second},
		{"discord body", nil, `{"message":"You are being rate limited.","retry_after":0.8,"global":false}`, 800 * time.Millisecond},
		{"telegram body", nil, `{"ok":false,"error_code":429,"parameters":{"retry_after":12}}`, 12 * time.Second},
		{"nothing", nil, `rate limited`, 0},
		{"capped", header("86400"), "", time.Hour},
		{"past date", header(now.Add(-time.Minute).Format(http.TimeFormat)), "", 0},
	} {
		if got := retryAfter(tc.h, []byte(tc.body), now); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestHTTP429IsARateLimitNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, `{"retry_after": 2}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	err := send(t, config.Notifier{Type: "slack", URL: srv.URL}, downEvent())
	var limit *RateLimitError
	var perm *PermanentError
	if !errors.As(err, &limit) || limit.After != 2*time.Second || errors.As(err, &perm) {
		t.Fatalf("got %v, want a rate limit of 2s", err)
	}
}

func TestRenderGroup(t *testing.T) {
	taipei, _ := time.LoadLocation("Asia/Taipei")
	up := downFor("db")
	up.Kind, up.Status, up.Duration = model.EventUp, model.StatusUp, 5*time.Minute
	evs := []model.Event{downFor("api"), up, downFor("web")}
	m := RenderGroup(evs, "zh-TW", taipei, "")
	if m.Title != "監控通知：2 個中斷、1 個已恢復" || m.Count != 3 || m.Severity != SeverityCritical || m.Link != "" {
		t.Errorf("title %q count %d severity %d link %q", m.Title, m.Count, m.Severity, m.Link)
	}
	want := "• 服務中斷：api · HTTP 503 Service Unavailable\n• 已恢復：db · 中斷 5 分鐘\n• 服務中斷：web · HTTP 503 Service Unavailable"
	if m.Body != want {
		t.Errorf("body:\n%s\nwant:\n%s", m.Body, want)
	}
	if one := RenderGroup(evs[:1], "en", time.UTC, ""); one.Title != "DOWN: api" || one.Count != 1 {
		t.Errorf("a group of one renders as a single alert: %+v", one)
	}
	if recovered := RenderGroup([]model.Event{up, up}, "en", time.UTC, ""); recovered.Severity != SeverityGood {
		t.Errorf("recoveries only: severity %d, want good", recovered.Severity)
	}
}

// concurrencySender records how many sends overlap.
type concurrencySender struct {
	mu            sync.Mutex
	inFlight, max int
	sent          int
}

func (c *concurrencySender) Send(context.Context, Message) error {
	c.mu.Lock()
	c.inFlight++
	c.max = max(c.max, c.inFlight)
	c.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	c.mu.Lock()
	c.inFlight--
	c.sent++
	c.mu.Unlock()
	return nil
}

// Without grouping, a chat notifier still sends one message at a time, so
// a rate limit is met by one request rather than a crowd of retries.
func TestUngroupedChatSendsOneAtATime(t *testing.T) {
	cfg := chatConfig(20)
	zero := config.Duration(0)
	cfg.Notifiers[0].GroupInterval = &zero
	s := &concurrencySender{}
	d, err := NewDispatcher(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewSender: func(config.Notifier) (Sender, error) { return s, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		d.Notify(downFor(fmt.Sprintf("m%d", i)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Close(ctx)
	if s.sent != 20 || s.max != 1 {
		t.Errorf("sent %d messages, at most %d at once; want 20, one at a time", s.sent, s.max)
	}
}

// Alerts waiting for their group's message show as pending in the delivery
// log, so they are visible, and marked failed if Pharos stops first.
func TestQueuedAlertsAreLoggedAsPending(t *testing.T) {
	h := newGroupHarness(t, chatConfig(4), &fakeSender{})
	h.d.Notify(downFor("m0"))
	h.s.waitFor(t, 1)
	for i := 1; i < 4; i++ {
		h.d.Notify(downFor(fmt.Sprintf("m%d", i)))
	}
	h.clock.BlockUntil(1, 5*time.Second)
	states := func() map[string]int {
		log, _ := h.store.Notifications(context.Background(), 10)
		m := map[string]int{}
		for _, l := range log {
			m[l.State]++
		}
		return m
	}
	if got := states(); got[store.NotifyDelivered] != 1 || got[store.NotifyPending] != 3 {
		t.Fatalf("while waiting: %v, want 1 delivered and 3 pending", got)
	}
	h.clock.Advance(10 * time.Second)
	h.s.waitFor(t, 2)
	h.d.Close(context.Background())
	if got := states(); got[store.NotifyDelivered] != 4 {
		t.Fatalf("after the group message: %v, want 4 delivered", got)
	}
}
