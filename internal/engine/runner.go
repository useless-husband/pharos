package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/probe"
)

// runner owns one monitor's schedule and state.
type runner struct {
	mu      sync.Mutex
	mon     config.Monitor
	prober  probe.Prober
	trigger chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}

	tracker      tracker
	paused       bool
	incident     *model.Incident
	lastCheck    *model.Check
	certExpiry   time.Time
	certWarned   time.Time
	lastReminder time.Time
	firstSeen    time.Time
	started      time.Time
	nextCheck    time.Time
	maint        *Window

	// push monitors
	lastPush   time.Time // last heartbeat received
	lastPushOK bool      // whether it reported success
}

// run is the loop of an active (probing) monitor.
func (e *Engine) run(ctx context.Context, r *runner) {
	r.mu.Lock()
	delay := jitter(r.mon.ID, r.mon.Interval.D())
	trig := r.trigger
	r.mu.Unlock()
	for {
		r.mu.Lock()
		r.nextCheck = e.clock.Now().Add(delay)
		r.mu.Unlock()
		timer := e.clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-trig:
			timer.Stop()
		}

		r.mu.Lock()
		paused, mon, prober := r.paused, r.mon, r.prober
		r.mu.Unlock()
		if paused {
			delay = mon.Interval.D()
			continue
		}

		if e.sem != nil {
			select {
			case e.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		start := e.clock.Now()
		c := prober.Probe(ctx)
		if e.sem != nil {
			<-e.sem
		}
		if ctx.Err() != nil {
			return // a check cut short by shutdown is not a failure
		}
		c.MonitorID, c.At = mon.ID, start
		e.handle(ctx, r, e.Config(), c)

		r.mu.Lock()
		if r.tracker.pending() || r.tracker.status == model.StatusDown {
			delay = mon.RetryInterval.D()
		} else {
			delay = mon.Interval.D()
		}
		r.mu.Unlock()
	}
}

// runPush is the loop of a push (heartbeat) monitor: it only watches the
// deadline; heartbeats arrive through Engine.Push.
func (e *Engine) runPush(ctx context.Context, r *runner) {
	r.mu.Lock()
	trig := r.trigger
	r.mu.Unlock()
	for {
		r.mu.Lock()
		tick := r.mon.Interval.D()
		r.nextCheck = e.clock.Now().Add(tick)
		r.mu.Unlock()
		timer := e.clock.NewTimer(tick)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-trig:
			timer.Stop()
		}
		e.tickPush(ctx, r)
	}
}

// pushDeadline is when a push monitor is late. A monitor that has never
// reported gets a full heartbeat window from start; after a restart, the
// last heartbeat still counts, but the job always gets at least the grace
// period after Pharos starts (its heartbeat may have been sent while Pharos
// was down). Caller holds r.mu.
func pushDeadline(r *runner) time.Time {
	window := r.mon.Heartbeat.D() + r.mon.Grace.D()
	if r.lastPush.IsZero() {
		return r.started.Add(window)
	}
	d := r.lastPush.Add(window)
	if floor := r.started.Add(r.mon.Grace.D()); d.Before(floor) {
		d = floor
	}
	return d
}

// tickPush re-evaluates a push monitor on its own schedule: maintenance
// starting and ending, a missed deadline, and reminders. Heartbeats
// themselves arrive through Engine.Push.
func (e *Engine) tickPush(ctx context.Context, r *runner) {
	ctx = context.WithoutCancel(ctx)
	cfg := e.Config()
	now := e.clock.Now()
	var events []model.Event
	r.mu.Lock()
	if r.paused {
		r.mu.Unlock()
		return
	}
	m := r.mon
	prev := r.tracker.status
	if w := maintenanceAt(cfg, m.ID, now); w != nil {
		if prev != model.StatusMaintenance {
			events = append(events, e.enterMaintenance(ctx, r, w)...)
		}
		r.maint = w
	} else {
		if prev == model.StatusMaintenance {
			e.exitMaintenance(ctx, r, now)
		}
		deadline := pushDeadline(r)
		switch {
		case now.After(deadline) && r.tracker.status != model.StatusDown:
			since := r.lastPush
			if since.IsZero() || since.Before(r.started) {
				since = r.started
			}
			miss := model.Check{MonitorID: m.ID, At: later(deadline, r.tracker.since), Status: model.StatusDown,
				Message: fmt.Sprintf("no heartbeat received for %s (expected every %s)", config.FormatDuration(now.Sub(since).Round(time.Minute)), m.Heartbeat)}
			if err := e.store.InsertCheck(ctx, miss); err != nil {
				e.log.Error("store check", "monitor", m.ID, "err", err)
			}
			r.lastCheck = &miss
			events = append(events, e.observe(ctx, r, cfg, miss)...)
		case !now.After(deadline) && r.tracker.status == model.StatusUnknown && r.lastPushOK:
			// A good heartbeat arrived during maintenance or before a
			// resume and is still within its deadline.
			r.tracker.reset(model.StatusUp, now)
			if _, err := e.persist(ctx, r, change{from: model.StatusUnknown, to: model.StatusUp, at: now}, ""); err != nil {
				r.tracker.reset(model.StatusUnknown, now)
			}
		default:
			if ev, ok := e.reminder(r, cfg, now); ok {
				events = append(events, ev)
			}
		}
	}
	status := r.tracker.status
	r.mu.Unlock()
	if status != prev {
		e.publishStatus(r, cfg, prev)
	}
	for _, ev := range events {
		e.notifier.Notify(ev)
	}
}

// Window is an active or upcoming maintenance window.
type Window struct {
	Name     string    `json:"name"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Monitors []string  `json:"monitors,omitempty"`
}

func maintenanceAt(cfg *config.Config, id string, t time.Time) *Window {
	for _, mw := range cfg.Maintenance {
		if !mw.Covers(id) {
			continue
		}
		if s, e, ok := mw.ActiveAt(t); ok {
			return &Window{Name: mw.Name, Start: s, End: e, Monitors: mw.Monitors}
		}
	}
	return nil
}

// Maintenance returns maintenance windows that are active now or start
// within the given horizon, soonest first.
func (e *Engine) Maintenance(within time.Duration) []Window {
	cfg := e.Config()
	now := e.clock.Now()
	var out []Window
	for _, mw := range cfg.Maintenance {
		if s, end, ok := mw.ActiveAt(now); ok {
			out = append(out, Window{Name: mw.Name, Start: s, End: end, Monitors: mw.Monitors})
			continue
		}
		if s, end, ok := mw.Next(now, within); ok {
			out = append(out, Window{Name: mw.Name, Start: s, End: end, Monitors: mw.Monitors})
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
