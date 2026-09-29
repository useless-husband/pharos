package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/clock"
	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/probe"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/uptime"
)

// script is a prober that returns programmed results; after the script
// runs out it repeats the last one.
type script struct {
	mu      sync.Mutex
	results []model.Check
	calls   int
}

func (s *script) Probe(context.Context) model.Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.calls, len(s.results)-1)
	s.calls++
	return s.results[i]
}

func (s *script) set(results ...model.Check) {
	s.mu.Lock()
	s.results, s.calls = results, 0
	s.mu.Unlock()
}

func (s *script) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func up() model.Check { return model.Check{Status: model.StatusUp, Latency: 80 * time.Millisecond} }
func downc(msg string) model.Check {
	return model.Check{Status: model.StatusDown, Message: msg, Latency: 5 * time.Millisecond}
}

type recorder struct {
	mu     sync.Mutex
	events []model.Event
}

func (r *recorder) Notify(ev model.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) kinds() []model.EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var k []model.EventKind
	for _, e := range r.events {
		k = append(k, e.Kind)
	}
	return k
}

func (r *recorder) last() model.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

type harness struct {
	t       *testing.T
	clock   *clock.Fake
	store   *store.Store
	engine  *Engine
	notes   *recorder
	probers map[string]*script
	cancel  context.CancelFunc
	cfg     *config.Config
	dbPath  string
}

var start = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

func baseConfig(monitors ...config.Monitor) *config.Config {
	cfg := &config.Config{
		Defaults: config.Defaults{Confirm: config.Confirm{Down: 2, Up: 2}, CertExpiryWarn: config.Duration(14 * 24 * time.Hour)},
		Storage:  config.Storage{Retention: config.Duration(30 * 24 * time.Hour)},
		Monitors: monitors,
	}
	return cfg
}

func httpMon(id string) config.Monitor {
	return config.Monitor{ID: id, Name: id, Type: config.TypeHTTP, URL: "https://" + id + ".example",
		Interval: config.Duration(time.Minute), Timeout: config.Duration(10 * time.Second), RetryInterval: config.Duration(10 * time.Second)}
}

func newHarness(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	h := &harness{t: t, clock: clock.NewFake(start), notes: &recorder{}, probers: map[string]*script{}, cfg: cfg,
		dbPath: filepath.Join(t.TempDir(), "pharos.db")}
	h.open()
	for _, m := range cfg.Monitors {
		if m.Type != config.TypePush {
			h.probers[m.ID] = &script{results: []model.Check{up()}}
		}
	}
	h.startEngine()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) open() {
	s, err := store.Open(context.Background(), h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = s
}

func (h *harness) startEngine() {
	h.engine = New(Options{
		Store: h.store, Notifier: h.notes, Clock: h.clock,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewProber: func(m config.Monitor) (probe.Prober, error) {
			p := h.probers[m.ID]
			if p == nil {
				p = &script{results: []model.Check{up()}}
				h.probers[m.ID] = p
			}
			return p, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	if err := h.engine.Start(ctx, h.cfg); err != nil {
		h.t.Fatal(err)
	}
	h.settle()
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		h.engine.Wait()
		h.cancel = nil
	}
	if h.store != nil {
		h.store.Close()
		h.store = nil
	}
}

// settle waits until every runner and the housekeeper are asleep again.
func (h *harness) settle() {
	h.t.Helper()
	want := len(h.engine.Config().Monitors) + 1
	if !h.clock.BlockUntil(want, 5*time.Second) {
		h.t.Fatalf("goroutines did not settle: %d waiters, want %d", h.clock.Waiters(), want)
	}
}

// advance moves time forward in small steps so every timer fires in order.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	const step = time.Second
	for d > 0 {
		s := min(step, d)
		h.clock.Advance(s)
		h.settle()
		d -= s
	}
}

// waitChecks waits in real time for a triggered (not timer-driven) check
// to finish, then for the runner to go back to sleep.
func (h *harness) waitChecks(p *script, n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.count() < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("triggered check did not run")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The runner stopped its timer to take the trigger; it re-arms one
	// after handling the result.
	h.settle()
}

// untilChecks advances time until p has run n more checks.
func (h *harness) untilChecks(p *script, n int) {
	h.t.Helper()
	target := p.count() + n
	for i := 0; p.count() < target; i++ {
		if i > 3600 {
			h.t.Fatalf("no check after an hour of simulated time")
		}
		h.advance(time.Second)
	}
}

func (h *harness) state(id string) MonitorState {
	st, ok := h.engine.State(id)
	if !ok {
		h.t.Fatalf("no monitor %s", id)
	}
	return st
}

func TestOutageLifecycle(t *testing.T) {
	h := newHarness(t, baseConfig(httpMon("web")))
	p := h.probers["web"]
	h.advance(15 * time.Second) // past the start-up jitter
	if st := h.state("web"); st.Status != model.StatusUp {
		t.Fatalf("status %s, want up", st.Status)
	}

	p.set(downc("HTTP 502 Bad Gateway"))
	calls := p.count()
	h.untilChecks(p, 1) // first failure
	if st := h.state("web"); st.Status != model.StatusUp {
		t.Fatal("one failure must not change status")
	}
	failStart := h.state("web").LastCheck.At
	h.advance(10 * time.Second) // retry interval: the second failure confirms
	st := h.state("web")
	if st.Status != model.StatusDown || st.Incident == nil || !st.Incident.Started.Equal(failStart) {
		t.Fatalf("state %+v, want down since %s", st, failStart)
	}
	if st.Incident.Cause != "HTTP 502 Bad Gateway" {
		t.Errorf("cause %q", st.Incident.Cause)
	}
	if p.count()-calls != 2 {
		t.Errorf("retry interval not used: %d checks", p.count()-calls)
	}
	if k := h.notes.kinds(); len(k) != 1 || k[0] != model.EventDown {
		t.Fatalf("notifications %v", k)
	}

	// While down the monitor keeps checking at the retry interval.
	h.advance(5 * time.Minute)
	p.set(up())
	h.untilChecks(p, 1)
	recoverAt := h.state("web").LastCheck.At
	h.untilChecks(p, 1)
	st = h.state("web")
	if st.Status != model.StatusUp || st.Incident != nil {
		t.Fatalf("not recovered: %+v", st)
	}
	ev := h.notes.last()
	if ev.Kind != model.EventUp || ev.Incident == nil || ev.Duration != recoverAt.Sub(failStart) {
		t.Fatalf("up event %+v, want duration %s", ev, recoverAt.Sub(failStart))
	}

	// History: availability counts exactly the confirmed outage.
	now := h.clock.Now()
	ps, _ := h.store.Periods(context.Background(), start, now)
	sum := uptime.Summarize(ps["web"], start, now, now)
	if sum.Down != recoverAt.Sub(failStart) {
		t.Errorf("downtime %s, want %s", sum.Down, recoverAt.Sub(failStart))
	}
	incs, _ := h.store.Incidents(context.Background(), store.IncidentQuery{})
	if len(incs) != 1 || incs[0].Resolution != "recovered" {
		t.Errorf("incidents %+v", incs)
	}
}

func TestDegradedEvents(t *testing.T) {
	h := newHarness(t, baseConfig(httpMon("api")))
	p := h.probers["api"]
	h.advance(15 * time.Second)
	p.set(model.Check{Status: model.StatusDegraded, Message: "slow response: 2.4s (limit 2s)"})
	h.advance(70 * time.Second)
	if st := h.state("api"); st.Status != model.StatusDegraded {
		t.Fatalf("status %s", st.Status)
	}
	if ev := h.notes.last(); ev.Kind != model.EventDegraded || ev.Message != "slow response: 2.4s (limit 2s)" {
		t.Fatalf("event %+v", ev)
	}
	p.set(up())
	h.advance(2 * time.Minute)
	if ev := h.notes.last(); ev.Kind != model.EventDegraded || ev.Status != model.StatusUp || ev.Message != "performance recovered" {
		t.Fatalf("recovery event %+v", ev)
	}
	if st := h.state("api"); st.Incident != nil {
		t.Error("degradation is not an incident")
	}
}

func TestMaintenanceSuppressesAlerts(t *testing.T) {
	cfg := baseConfig(httpMon("db"))
	cfg.Maintenance = []config.Maintenance{{Name: "upgrade", Start: start.Add(10 * time.Minute).Format(time.RFC3339), End: start.Add(40 * time.Minute).Format(time.RFC3339)}}
	reparse(t, cfg)
	h := newHarness(t, cfg)
	p := h.probers["db"]
	h.advance(10*time.Minute + 30*time.Second) // the window has started
	p.set(downc("connection refused"))
	h.advance(25 * time.Minute) // inside the window, failing
	st := h.state("db")
	if st.Status != model.StatusMaintenance || st.Maintenance == nil || st.Maintenance.Name != "upgrade" {
		t.Fatalf("state %+v", st)
	}
	if len(h.notes.kinds()) != 0 {
		t.Fatalf("no alerts during maintenance, got %v", h.notes.kinds())
	}
	p.set(up())
	h.advance(10 * time.Minute) // window ends at +40m
	if st := h.state("db"); st.Status != model.StatusUp {
		t.Fatalf("after maintenance: %s", st.Status)
	}
	now := h.clock.Now()
	ps, _ := h.store.Periods(context.Background(), start, now)
	sum := uptime.Summarize(ps["db"], start, now, now)
	if sum.Down != 0 || sum.Maintenance < 29*time.Minute {
		t.Errorf("summary %+v", sum)
	}
}

func TestPauseAndResume(t *testing.T) {
	h := newHarness(t, baseConfig(httpMon("web")))
	p := h.probers["web"]
	h.advance(15 * time.Second)
	p.set(downc("refused"))
	h.advance(80 * time.Second)
	if h.state("web").Incident == nil {
		t.Fatal("expected an open incident")
	}
	if err := h.engine.SetPaused(context.Background(), "web", true); err != nil {
		t.Fatal(err)
	}
	h.settle()
	calls := p.count()
	h.advance(5 * time.Minute)
	if p.count() != calls {
		t.Error("paused monitors must not be checked")
	}
	st := h.state("web")
	if st.Status != model.StatusPaused || st.Incident != nil {
		t.Fatalf("paused state %+v", st)
	}
	incs, _ := h.store.Incidents(context.Background(), store.IncidentQuery{})
	if incs[0].Resolution != "paused" {
		t.Errorf("incident resolution %q", incs[0].Resolution)
	}
	p.set(up())
	calls = p.count()
	if err := h.engine.SetPaused(context.Background(), "web", false); err != nil {
		t.Fatal(err)
	}
	h.waitChecks(p, calls+1) // resuming triggers an immediate check
	if st := h.state("web"); st.Status != model.StatusUp {
		t.Fatalf("resumed status %s", st.Status)
	}
	if err := h.engine.SetPaused(context.Background(), "nope", true); err != ErrNotFound {
		t.Errorf("unknown monitor: %v", err)
	}
}

func TestPushMonitor(t *testing.T) {
	push := config.Monitor{ID: "backup", Name: "Nightly backup", Type: config.TypePush,
		Heartbeat: config.Duration(time.Hour), Grace: config.Duration(5 * time.Minute), Interval: config.Duration(30 * time.Second)}
	h := newHarness(t, baseConfig(push))
	token := h.engine.PushToken(push)
	if len(token) != 32 || token != h.engine.PushToken(push) {
		t.Fatalf("token %q", token)
	}
	if _, err := h.engine.Push("wrong", true, "", 0); err != ErrNotFound {
		t.Fatalf("wrong token: %v", err)
	}
	if id, err := h.engine.Push(token, true, "", 0); err != nil || id != "backup" {
		t.Fatal(err)
	}
	if st := h.state("backup"); st.Status != model.StatusUp {
		t.Fatalf("after heartbeat: %s", st.Status)
	}
	h.advance(64 * time.Minute) // inside heartbeat + grace
	if st := h.state("backup"); st.Status != model.StatusUp {
		t.Fatalf("still within grace: %s", st.Status)
	}
	h.advance(2 * time.Minute)
	st := h.state("backup")
	if st.Status != model.StatusDown || st.Incident == nil {
		t.Fatalf("missed heartbeat: %+v", st)
	}
	if ev := h.notes.last(); ev.Kind != model.EventDown || ev.Message == "" {
		t.Fatalf("down event %+v", ev)
	}
	// The job reports its own failure, then succeeds.
	if _, err := h.engine.Push(token, true, "", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if st := h.state("backup"); st.Status != model.StatusUp || st.LastCheck.Latency != 1500*time.Millisecond {
		t.Fatalf("recovered: %+v", st)
	}
	_, _ = h.engine.Push(token, false, "exit status 2", 0)
	if st := h.state("backup"); st.Status != model.StatusDown || st.Incident.Cause != "exit status 2" {
		t.Fatalf("reported failure: %+v", st)
	}
}

func TestReminders(t *testing.T) {
	cfg := baseConfig(httpMon("web"))
	cfg.Defaults.RemindEvery = config.Duration(30 * time.Minute)
	h := newHarness(t, cfg)
	h.probers["web"].set(downc("refused"))
	h.advance(90 * time.Minute)
	var reminders int
	for _, k := range h.notes.kinds() {
		if k == model.EventReminder {
			reminders++
		}
	}
	if reminders != 2 {
		t.Errorf("reminders = %d, want 2 in 90 minutes", reminders)
	}
}

func TestCertificateWarnings(t *testing.T) {
	h := newHarness(t, baseConfig(httpMon("web")))
	expiry := start.Add(10 * 24 * time.Hour)
	c := up()
	c.CertExpiry = expiry
	h.probers["web"].set(c)
	h.advance(3 * time.Hour)
	count := func() (n int) {
		for _, k := range h.notes.kinds() {
			if k == model.EventCert {
				n++
			}
		}
		return
	}
	if count() != 1 {
		t.Fatalf("cert warnings in first 3h = %d, want 1", count())
	}
	if ev := h.notes.last(); !ev.CertExpiry.Equal(expiry) {
		t.Errorf("event %+v", ev)
	}
	h.advance(22 * time.Hour)
	if count() != 2 {
		t.Fatalf("cert warnings after a day = %d, want 2", count())
	}
	// Renewal re-arms the warning.
	renewed := up()
	renewed.CertExpiry = start.Add(90 * 24 * time.Hour)
	h.probers["web"].set(renewed)
	h.advance(2 * time.Minute)
	if st := h.state("web"); !st.CertExpiry.Equal(renewed.CertExpiry) {
		t.Errorf("cert expiry %s", st.CertExpiry)
	}
	states, _ := h.store.LoadStates(context.Background())
	if !states["web"].CertWarned.IsZero() {
		t.Error("renewal should clear cert_warned")
	}
}

func TestRestartExcludesGapAndKeepsOutage(t *testing.T) {
	cfg := baseConfig(httpMon("web"), httpMon("api"))
	h := newHarness(t, cfg)
	h.probers["api"].set(downc("refused"))
	h.advance(3 * time.Minute)
	if h.state("api").Status != model.StatusDown || h.state("web").Status != model.StatusUp {
		t.Fatal("setup")
	}
	apiIncident := h.state("api").Incident.ID
	lastWeb := h.state("web").LastCheck.At

	// Pharos is stopped for an hour.
	h.cancel()
	h.engine.Wait()
	h.cancel = nil
	h.clock.Advance(time.Hour)
	h.startEngine()

	if st := h.state("web"); st.Status != model.StatusUnknown {
		t.Fatalf("web after restart: %s (unobserved time must be unknown)", st.Status)
	}
	if st := h.state("api"); st.Status != model.StatusDown || st.Incident == nil || st.Incident.ID != apiIncident {
		t.Fatalf("api after restart: %+v", st)
	}
	h.advance(15 * time.Second)
	if st := h.state("web"); st.Status != model.StatusUp {
		t.Fatalf("web after first check: %s", st.Status)
	}
	now := h.clock.Now()
	ps, _ := h.store.Periods(context.Background(), start, now)
	sum := uptime.Summarize(ps["web"], start, now, now)
	gap := sum.Up + sum.Down + sum.Degraded
	if gap > now.Sub(start)-time.Hour+time.Minute || sum.Down != 0 {
		t.Errorf("web counted %s of %s; the offline hour after %s must be excluded", gap, now.Sub(start), lastWeb)
	}
	var down int
	for _, k := range h.notes.kinds() {
		if k == model.EventDown {
			down++
		}
	}
	if down != 1 {
		t.Errorf("restart must not re-send the down alert, got %d", down)
	}
}

func TestReload(t *testing.T) {
	h := newHarness(t, baseConfig(httpMon("web"), httpMon("old")))
	h.advance(15 * time.Second)

	next := baseConfig(httpMon("web"), httpMon("new"))
	next.Monitors[0].URL = "https://changed.example" // web changes
	if err := h.engine.Reload(next); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.engine.State("old"); ok {
		t.Error("removed monitor still present")
	}
	if st := h.state("web"); st.Status != model.StatusUp || st.Monitor.URL != "https://changed.example" {
		t.Errorf("changed monitor lost state: %+v", st)
	}
	h.advance(15 * time.Second)
	if st := h.state("new"); st.Status != model.StatusUp {
		t.Errorf("added monitor: %s", st.Status)
	}
	now := h.clock.Now()
	ps, _ := h.store.Periods(context.Background(), start, now.Add(time.Hour))
	for _, p := range ps["old"] {
		if p.End.IsZero() && p.Status.Counted() {
			t.Errorf("removed monitor still accrues %s time", p.Status)
		}
	}
}

func TestCheckNow(t *testing.T) {
	h := newHarness(t, baseConfig(httpMon("web")))
	h.advance(15 * time.Second)
	before := h.probers["web"].count()
	if err := h.engine.CheckNow("web"); err != nil {
		t.Fatal(err)
	}
	h.waitChecks(h.probers["web"], before+1)
	if got := h.probers["web"].count(); got != before+1 {
		t.Errorf("CheckNow ran %d checks, want 1", got-before)
	}
	if err := h.engine.CheckNow("nope"); err != ErrNotFound {
		t.Errorf("unknown: %v", err)
	}
}

func TestHubDropsForSlowSubscribers(t *testing.T) {
	hub := newHub()
	ch, cancel := hub.Subscribe()
	for i := 0; i < 200; i++ {
		hub.Publish(model.Event{Kind: model.EventCheck})
	}
	if len(ch) != cap(ch) {
		t.Errorf("buffer %d/%d", len(ch), cap(ch))
	}
	cancel()
	cancel() // idempotent
}

// reparse runs the config through validation so maintenance windows compile.
func reparse(t *testing.T, cfg *config.Config) {
	t.Helper()
	for i := range cfg.Maintenance {
		if err := config.CompileMaintenance(&cfg.Maintenance[i], time.UTC); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPushMonitorSurvivesRestart(t *testing.T) {
	push := config.Monitor{ID: "backup", Name: "Nightly backup", Type: config.TypePush,
		Heartbeat: config.Duration(24 * time.Hour), Grace: config.Duration(time.Hour), Interval: config.Duration(30 * time.Second)}
	h := newHarness(t, baseConfig(push))
	if _, err := h.engine.Push(h.engine.PushToken(push), true, "", 0); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Hour)
	h.cancel()
	h.engine.Wait()
	h.cancel = nil
	h.clock.Advance(30 * time.Minute)
	h.startEngine()
	if st := h.state("backup"); st.Status != model.StatusUp {
		t.Fatalf("a push monitor between heartbeats must stay up across a restart, got %s", st.Status)
	}
	// The deadline restarts from the restart: 25h later without a heartbeat it is down.
	h.advance(24*time.Hour + 61*time.Minute)
	if st := h.state("backup"); st.Status != model.StatusDown {
		t.Fatalf("missed heartbeat after restart: %s", st.Status)
	}
}
