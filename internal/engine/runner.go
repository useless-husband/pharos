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
	lastPush   time.Time
	pushMissed bool
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

		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		start := e.clock.Now()
		c := prober.Probe(ctx)
		<-e.sem
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
		e.checkHeartbeat(ctx, r)
	}
}

func (e *Engine) checkHeartbeat(ctx context.Context, r *runner) {
	now := e.clock.Now()
	r.mu.Lock()
	m := r.mon
	last := r.lastPush
	if last.IsZero() || last.Before(r.started) {
		// No heartbeat since this runner started: give the job a full
		// window from the start before calling it late.
		last = r.started
	}
	deadline := last.Add(m.Heartbeat.D() + m.Grace.D())
	missed := !r.paused && now.After(deadline) && !r.pushMissed
	if missed {
		r.pushMissed = true
	}
	r.mu.Unlock()
	if !missed {
		return
	}
	c := model.Check{
		MonitorID: m.ID,
		At:        deadline,
		Status:    model.StatusDown,
		Message:   fmt.Sprintf("no heartbeat received for %s (expected every %s)", config.FormatDuration(now.Sub(last).Round(time.Second)), m.Heartbeat),
	}
	e.handle(ctx, r, e.Config(), c)
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
