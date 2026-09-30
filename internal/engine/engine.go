// Package engine schedules checks, confirms status changes, records history
// and raises events.
package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/useless-husband/pharos/internal/clock"
	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/probe"
	"github.com/useless-husband/pharos/internal/store"
)

// Notifier receives events that may need to be delivered to people.
// Implementations must not block.
type Notifier interface {
	Notify(model.Event)
}

// Options configure an Engine.
type Options struct {
	Store    *store.Store
	Notifier Notifier
	Clock    clock.Clock
	Logger   *slog.Logger
	// MaxConcurrent bounds how many checks run at once across monitors.
	// Zero, the default, means no bound beyond one check per monitor: a
	// bound below the number of monitors makes checks queue behind timeouts
	// exactly when many targets are unreachable, and delays the alerts.
	MaxConcurrent int
	// NewProber builds probers; tests replace it.
	NewProber func(config.Monitor) (probe.Prober, error)
	// ReadOnly loads state for display without running checks or writing
	// to the database (used by `pharos export`).
	ReadOnly bool
}

// Engine runs every monitor. It is safe for concurrent use.
type Engine struct {
	store     *store.Store
	notifier  Notifier
	clock     clock.Clock
	log       *slog.Logger
	newProber func(config.Monitor) (probe.Prober, error)
	sem       chan struct{}
	hub       *Hub
	readOnly  bool

	// cfg is read lock-free: runners read it on every check, and must never
	// wait on mu (Reload holds it while stopping runners).
	cfg      atomic.Pointer[config.Config]
	reloadMu sync.Mutex // serializes Reload

	mu      sync.RWMutex // guards runners, order, ctx
	runners map[string]*runner
	order   []string // monitor ids in config order
	ctx     context.Context
	wg      sync.WaitGroup

	pushKey []byte
	aliveAt time.Time // last liveness mark before this start
}

// ErrNotFound is returned for unknown monitor ids and push tokens.
var ErrNotFound = errors.New("monitor not found")

// New creates an engine. Call Start to begin checking.
func New(opts Options) *Engine {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NewProber == nil {
		opts.NewProber = probe.New
	}
	if opts.Notifier == nil {
		opts.Notifier = nopNotifier{}
	}
	return &Engine{
		store:     opts.Store,
		notifier:  opts.Notifier,
		clock:     opts.Clock,
		log:       opts.Logger,
		newProber: opts.NewProber,
		sem:       semaphore(opts.MaxConcurrent),
		hub:       newHub(),
		runners:   map[string]*runner{},
		readOnly:  opts.ReadOnly,
	}
}

type nopNotifier struct{}

func (nopNotifier) Notify(model.Event) {}

// Hub returns the live event hub.
func (e *Engine) Hub() *Hub { return e.hub }

// Start restores persisted state, starts one runner per monitor and the
// maintenance jobs. It returns immediately; runners stop when ctx ends.
// Call Wait after cancelling ctx for a clean shutdown.
func (e *Engine) Start(ctx context.Context, cfg *config.Config) error {
	key, err := e.store.Secret(ctx, "push")
	if err != nil {
		return fmt.Errorf("load push secret: %w", err)
	}
	e.pushKey = key
	states, err := e.store.LoadStates(ctx)
	if err != nil {
		return fmt.Errorf("load monitor state: %w", err)
	}
	now := e.clock.Now()
	aliveAt, err := e.store.AliveAt(ctx)
	if err != nil {
		return fmt.Errorf("load liveness: %w", err)
	}

	e.mu.Lock()
	e.ctx = ctx
	e.aliveAt = aliveAt
	e.mu.Unlock()
	e.cfg.Store(cfg)

	// Monitors that were removed from the configuration while Pharos was
	// stopped: close their open history so they stop accruing time.
	for id, st := range states {
		if e.readOnly {
			break
		}
		if _, ok := cfg.MonitorByID(id); !ok && st.Status != model.StatusUnknown && !st.Since.IsZero() {
			e.retire(ctx, id, lastSeen(st, aliveAt, now))
		}
	}

	e.mu.Lock()
	for _, m := range cfg.Monitors {
		r, err := e.newRunner(ctx, m, states[m.ID], now)
		if err != nil {
			e.mu.Unlock()
			return err
		}
		e.runners[m.ID] = r
		e.order = append(e.order, m.ID)
	}
	if e.readOnly {
		e.mu.Unlock()
		return nil
	}
	// Fold in a write-ahead log left by the previous run before checks
	// start writing, rather than stalling the first round.
	if err := e.store.Checkpoint(ctx); err != nil {
		e.log.Warn("checkpoint", "err", err)
	}
	for _, id := range e.order {
		e.launch(e.runners[id])
	}
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.housekeeping(ctx)
	}()
	e.log.Info("engine started", "monitors", len(cfg.Monitors))
	return nil
}

// Wait blocks until every runner has stopped.
func (e *Engine) Wait() { e.wg.Wait() }

// lastSeen is the last moment Pharos is known to have been watching a
// monitor: its last check, or the last liveness mark if that is later
// (between checks the confirmed status is still known).
func lastSeen(st *store.MonitorState, aliveAt, now time.Time) time.Time {
	t := st.Since
	if st.LastCheck != nil && st.LastCheck.At.After(t) {
		t = st.LastCheck.At
	}
	if aliveAt.After(t) {
		t = aliveAt
	}
	if t.IsZero() || t.After(now) {
		t = now
	}
	return t
}

func (e *Engine) retire(ctx context.Context, id string, at time.Time) {
	if _, err := e.store.ApplyTransition(ctx, store.Transition{MonitorID: id, To: model.StatusUnknown, At: at, Resolution: "monitor removed from configuration"}); err != nil {
		e.log.Error("retire monitor", "monitor", id, "err", err)
	}
}

// newRunner builds a runner, restoring persisted state.
func (e *Engine) newRunner(ctx context.Context, m config.Monitor, st *store.MonitorState, now time.Time) (*runner, error) {
	r := &runner{mon: m, trigger: make(chan struct{}, 1), started: now}
	if m.Type != config.TypePush {
		p, err := e.newProber(m)
		if err != nil {
			return nil, fmt.Errorf("monitor %s: %w", m.ID, err)
		}
		r.prober = p
	}
	if !e.readOnly {
		if err := e.store.EnsureMonitor(ctx, m.ID, now); err != nil {
			return nil, err
		}
	}
	r.firstSeen = now
	if st == nil {
		r.tracker.reset(model.StatusUnknown, now)
		return r, nil
	}
	r.firstSeen = st.FirstSeen
	r.paused = st.Paused
	r.certWarned = st.CertWarned
	r.incident = st.Incident
	if r.incident != nil {
		r.lastReminder = now // do not fire a reminder the moment we restart
	}
	r.lastCheck = st.LastCheck
	if r.lastCheck != nil {
		r.certExpiry = r.lastCheck.CertExpiry
		if m.Type == config.TypePush && r.lastCheck.Status != model.StatusDown {
			// The last good heartbeat anchors the deadline across restarts.
			r.lastPush, r.lastPushOK = r.lastCheck.At, true
		}
	}
	if e.readOnly {
		r.tracker.reset(st.Status, st.Since)
		if st.Paused {
			r.tracker.reset(model.StatusPaused, st.Since)
		}
		return r, nil
	}
	switch {
	case st.Paused:
		r.tracker.reset(model.StatusPaused, st.Since)
		if st.Status != model.StatusPaused {
			e.persist(ctx, r, change{to: model.StatusPaused, at: now}, "paused")
		}
	case st.Status == model.StatusDown:
		// Keep an ongoing outage open across the restart.
		r.tracker.reset(model.StatusDown, st.Since)
	case m.Type == config.TypePush && st.Status.Available():
		// A push monitor's state is defined by its heartbeat deadline
		// (see pushDeadline), which covers the restart: keep it.
		r.tracker.reset(st.Status, st.Since)
	case st.Status == model.StatusUp || st.Status == model.StatusDegraded || st.Status == model.StatusMaintenance:
		// Pharos itself was not watching between its last observation and
		// now: record that gap as unknown so it is not counted as uptime.
		at := lastSeen(st, e.aliveAt, now)
		r.tracker.reset(model.StatusUnknown, at)
		e.persist(ctx, r, change{from: st.Status, to: model.StatusUnknown, at: at}, "")
	default:
		r.tracker.reset(model.StatusUnknown, now)
	}
	return r, nil
}

// launch starts a runner's goroutine. Must hold e.mu.
func (e *Engine) launch(r *runner) {
	ctx, cancel := context.WithCancel(e.ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(r.done)
		if r.mon.Type == config.TypePush {
			e.runPush(ctx, r)
		} else {
			e.run(ctx, r)
		}
	}()
}

// Reload applies a new configuration: added monitors start, removed ones
// stop (their history is kept), and changed ones restart with their state.
//
// Everything that can fail is prepared first, so a failed reload changes
// nothing. Runners are stopped without holding e.mu, so the dashboard keeps
// answering while a slow check finishes.
func (e *Engine) Reload(cfg *config.Config) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	now := e.clock.Now()
	e.mu.RLock()
	ctx := e.ctx
	current := make(map[string]*runner, len(e.runners))
	for id, r := range e.runners {
		current[id] = r
	}
	e.mu.RUnlock()
	// A monitor may come back after being removed; restore its history.
	states, err := e.store.LoadStates(ctx)
	if err != nil {
		return err
	}

	type swap struct {
		r      *runner
		mon    config.Monitor
		prober probe.Prober
	}
	next := map[string]*runner{}
	var order []string
	var fresh []*runner
	var swaps []swap
	for _, m := range cfg.Monitors {
		order = append(order, m.ID)
		old := current[m.ID]
		var oldMon config.Monitor
		if old != nil {
			old.mu.Lock()
			oldMon = old.mon
			old.mu.Unlock()
		}
		switch {
		case old != nil && oldMon.Fingerprint() == m.Fingerprint():
			next[m.ID] = old
		case old == nil:
			r, err := e.newRunner(ctx, m, states[m.ID], now)
			if err != nil {
				return err
			}
			next[m.ID] = r
			fresh = append(fresh, r)
		default:
			var p probe.Prober
			if m.Type != config.TypePush {
				if p, err = e.newProber(m); err != nil {
					return fmt.Errorf("monitor %s: %w", m.ID, err)
				}
			}
			next[m.ID] = old
			swaps = append(swaps, swap{old, m, p})
		}
	}
	var removed []*runner
	for id, r := range current {
		if _, keep := next[id]; !keep {
			removed = append(removed, r)
		}
	}

	// Stop what changes or goes away.
	for _, sw := range swaps {
		sw.r.cancel()
	}
	for _, r := range removed {
		r.cancel()
	}
	for _, sw := range swaps {
		<-sw.r.done
	}
	for _, r := range removed {
		<-r.done
	}

	// Apply.
	for _, sw := range swaps {
		sw.r.mu.Lock()
		if sw.r.mon.Type != sw.mon.Type || sw.r.mon.Heartbeat != sw.mon.Heartbeat {
			// A new kind of check starts its deadlines from now.
			sw.r.started, sw.r.lastPush, sw.r.lastPushOK = now, time.Time{}, false
		}
		sw.r.mon, sw.r.prober = sw.mon, sw.prober
		sw.r.trigger = make(chan struct{}, 1)
		sw.r.mu.Unlock()
	}
	for _, r := range removed {
		r.mu.Lock()
		if r.incident != nil || r.tracker.status != model.StatusUnknown {
			e.retire(ctx, r.mon.ID, now)
		}
		r.mu.Unlock()
	}
	e.cfg.Store(cfg)
	e.mu.Lock()
	e.runners, e.order = next, order
	for _, sw := range swaps {
		e.launch(sw.r)
	}
	for _, r := range fresh {
		e.launch(r)
	}
	e.mu.Unlock()
	e.log.Info("configuration reloaded", "added", len(fresh), "changed", len(swaps), "removed", len(removed), "monitors", len(order))
	return nil
}

// Config returns the active configuration. It never blocks.
func (e *Engine) Config() *config.Config { return e.cfg.Load() }

func (e *Engine) runner(id string) (*runner, *config.Config) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.runners[id], e.cfg.Load()
}

// CheckNow runs a check as soon as possible.
func (e *Engine) CheckNow(id string) error {
	r, _ := e.runner(id)
	if r == nil {
		return ErrNotFound
	}
	r.mu.Lock()
	trig := r.trigger
	r.mu.Unlock()
	select {
	case trig <- struct{}{}:
	default:
	}
	return nil
}

// SetPaused pauses or resumes a monitor. Paused monitors are not checked,
// raise no alerts and are excluded from availability.
func (e *Engine) SetPaused(ctx context.Context, id string, paused bool) error {
	r, cfg := e.runner(id)
	if r == nil {
		return ErrNotFound
	}
	if err := e.store.SetPaused(ctx, id, paused); err != nil {
		return err
	}
	now := e.clock.Now()
	r.mu.Lock()
	if r.paused == paused {
		r.mu.Unlock()
		return nil
	}
	r.paused = paused
	prev := r.tracker.status
	if paused {
		r.tracker.reset(model.StatusPaused, now)
		_, _ = e.persist(ctx, r, change{from: prev, to: model.StatusPaused, at: now}, "paused")
	} else {
		r.tracker.reset(model.StatusUnknown, now)
		r.maint = nil
		_, _ = e.persist(ctx, r, change{from: prev, to: model.StatusUnknown, at: now}, "")
	}
	r.mu.Unlock()
	e.publishStatus(r, cfg, prev)
	if !paused {
		_ = e.CheckNow(id)
	}
	return nil
}

// PushToken returns the heartbeat token of a push monitor.
func (e *Engine) PushToken(m config.Monitor) string {
	if m.Token != "" {
		return m.Token
	}
	mac := hmac.New(sha256.New, e.pushKey)
	mac.Write([]byte("push:" + m.ID))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// Push records a heartbeat for the push monitor owning token. A heartbeat
// can report failure (ok=false) with a message, for jobs that know they failed.
// The heartbeat is recorded under the engine's lifetime rather than the
// HTTP request's, so a client hanging up cannot cancel the write.
func (e *Engine) Push(token string, ok bool, msg string, latency time.Duration) (string, error) {
	e.mu.RLock()
	var r *runner
	for _, id := range e.order {
		cand := e.runners[id]
		cand.mu.Lock()
		m := cand.mon
		cand.mu.Unlock()
		if m.Type == config.TypePush && subtle.ConstantTimeCompare([]byte(e.PushToken(m)), []byte(token)) == 1 {
			r = cand
		}
	}
	engineCtx := e.ctx
	e.mu.RUnlock()
	cfg := e.cfg.Load()
	if r == nil {
		return "", ErrNotFound
	}
	now := e.clock.Now()
	c := model.Check{MonitorID: r.mon.ID, At: now, Status: model.StatusUp, Latency: latency, Message: msg}
	if !ok {
		c.Status = model.StatusDown
		if c.Message == "" {
			c.Message = "job reported failure"
		}
	}
	r.mu.Lock()
	r.lastPush, r.lastPushOK = now, ok
	paused, id := r.paused, r.mon.ID
	r.mu.Unlock()
	if paused {
		return id, nil
	}
	e.handle(engineCtx, r, cfg, c)
	return id, nil
}

// semaphore returns a channel with n slots, or nil (no bound) for n <= 0.
func semaphore(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// jitter spreads first checks over up to 10s so a restart does not fire
// every monitor at the same instant. It is stable per monitor id.
func jitter(id string, interval time.Duration) time.Duration {
	// A 64-bit hash: a 32-bit one tops out at 4.29e9 ns, so jitter would
	// never exceed 4.3s whatever the span.
	h := fnv.New64a()
	h.Write([]byte(id))
	span := min(interval, 10*time.Second)
	if span <= 0 {
		return 0
	}
	return time.Duration(h.Sum64() % uint64(span))
}
