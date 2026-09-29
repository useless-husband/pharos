package engine

import (
	"context"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
)

// handle records a check and applies its consequences: maintenance
// transitions, confirmed status changes, reminders and certificate warnings.
func (e *Engine) handle(ctx context.Context, r *runner, cfg *config.Config, c model.Check) {
	// Finish bookkeeping even if the runner is being stopped mid-check: a
	// half-applied result would leave memory and the database disagreeing.
	ctx = context.WithoutCancel(ctx)
	var events []model.Event
	r.mu.Lock()
	m := r.mon
	maint := maintenanceAt(cfg, m.ID, c.At)
	c.Maintenance = maint != nil
	if err := e.store.InsertCheck(ctx, c); err != nil {
		e.log.Error("store check", "monitor", m.ID, "err", err)
	}
	r.lastCheck = &c
	if !c.CertExpiry.IsZero() {
		r.certExpiry = c.CertExpiry
	}
	prev := r.tracker.status

	switch {
	case r.paused:
		// A heartbeat or in-flight check that raced with a pause.
	case maint != nil:
		if prev != model.StatusMaintenance {
			events = append(events, e.enterMaintenance(ctx, r, maint)...)
		}
		r.maint = maint
	default:
		if prev == model.StatusMaintenance {
			e.exitMaintenance(ctx, r, c.At)
		}
		events = append(events, e.observe(ctx, r, cfg, c)...)
	}
	status := r.tracker.status
	r.mu.Unlock()

	e.hub.Publish(model.Event{Kind: model.EventCheck, At: c.At, Monitor: ref(m), Status: status, Check: &c})
	if status != prev {
		e.publishStatus(r, cfg, prev)
	}
	for _, ev := range events {
		e.notifier.Notify(ev)
	}
}

// observe feeds a check to the tracker and turns a confirmed change into
// events. If the change cannot be stored, the tracker is rolled back so the
// next check tries again and memory never disagrees with the database.
// Caller holds r.mu.
func (e *Engine) observe(ctx context.Context, r *runner, cfg *config.Config, c model.Check) []model.Event {
	var events []model.Event
	th := cfg.ConfirmFor(r.mon)
	if r.mon.Type == config.TypePush {
		th = config.Confirm{Down: 1, Up: 1} // the grace period already confirmed it
	}
	saved := r.tracker
	if ch, ok := r.tracker.observe(c, th); ok {
		closed, err := e.persist(ctx, r, ch, "recovered")
		if err != nil {
			r.tracker = saved
			return nil
		}
		events = append(events, e.transitionEvents(r, ch, closed)...)
	}
	if ev, ok := e.reminder(r, cfg, c.At); ok {
		events = append(events, ev)
	}
	if ev, ok := e.certWarning(ctx, r, cfg, c.At); ok {
		events = append(events, ev)
	}
	return events
}

// enterMaintenance moves a monitor into maintenance. An open incident is
// closed, and whoever was alerted is told why. Caller holds r.mu.
func (e *Engine) enterMaintenance(ctx context.Context, r *runner, w *Window) []model.Event {
	prev := r.tracker.status
	at := later(w.Start, r.tracker.since)
	r.tracker.reset(model.StatusMaintenance, at)
	r.maint = w
	closed, _ := e.persist(ctx, r, change{from: prev, to: model.StatusMaintenance, at: at}, "maintenance started")
	if closed == nil {
		return nil
	}
	return []model.Event{{Kind: model.EventUp, At: at, Monitor: ref(r.mon), Status: model.StatusMaintenance, Previous: prev,
		Message: w.Name, Incident: closed, Duration: closed.Duration(at)}}
}

// exitMaintenance ends maintenance at the window's end (or now, if the
// window was shortened). The status is unknown until the next result.
// Caller holds r.mu.
func (e *Engine) exitMaintenance(ctx context.Context, r *runner, now time.Time) {
	end := now
	if r.maint != nil && r.maint.End.Before(end) {
		end = r.maint.End
	}
	r.maint = nil
	prev := r.tracker.status
	r.tracker.reset(model.StatusUnknown, end)
	_, _ = e.persist(ctx, r, change{from: prev, to: model.StatusUnknown, at: end}, "")
}

// persist writes a confirmed change and keeps the runner's incident in
// sync. It returns the incident the change closed, if any. Caller holds r.mu.
func (e *Engine) persist(ctx context.Context, r *runner, ch change, resolution string) (*model.Incident, error) {
	inc, err := e.store.ApplyTransition(ctx, store.Transition{
		MonitorID: r.mon.ID, To: ch.to, At: ch.at, Cause: ch.cause, Resolution: resolution,
	})
	if err != nil {
		e.log.Error("store transition", "monitor", r.mon.ID, "to", ch.to, "err", err)
		return nil, err
	}
	e.log.Info("status changed", "monitor", r.mon.ID, "from", ch.from, "to", ch.to, "at", ch.at.Format(time.RFC3339), "cause", ch.cause)
	if inc == nil {
		return nil, nil
	}
	if inc.Ongoing() {
		r.incident = inc
		r.lastReminder = inc.Started
		return nil, nil
	}
	r.incident = nil
	return inc, nil
}

func ref(m config.Monitor) model.MonitorRef {
	return model.MonitorRef{ID: m.ID, Name: m.Name, Type: m.Type, Target: m.Target()}
}

// transitionEvents maps a confirmed change to notifications. Caller holds r.mu.
func (e *Engine) transitionEvents(r *runner, ch change, closed *model.Incident) []model.Event {
	ev := model.Event{At: ch.at, Monitor: ref(r.mon), Status: ch.to, Previous: ch.from, Message: ch.cause}
	switch {
	case ch.to == model.StatusDown:
		ev.Kind, ev.Incident = model.EventDown, r.incident
	case ch.from == model.StatusDown:
		ev.Kind, ev.Message = model.EventUp, ""
		if closed != nil {
			ev.Incident, ev.Duration = closed, closed.Duration(ch.at)
		}
	case ch.to == model.StatusDegraded && ch.from == model.StatusUp:
		ev.Kind = model.EventDegraded
	case ch.to == model.StatusUp && ch.from == model.StatusDegraded:
		ev.Kind, ev.Message = model.EventDegraded, "performance recovered"
	default:
		return nil // e.g. unknown -> up at startup
	}
	return []model.Event{ev}
}

func (e *Engine) reminder(r *runner, cfg *config.Config, now time.Time) (model.Event, bool) {
	every := cfg.RemindEveryFor(r.mon)
	if every <= 0 || r.tracker.status != model.StatusDown || r.incident == nil {
		return model.Event{}, false
	}
	if r.lastReminder.IsZero() {
		r.lastReminder = r.incident.Started
	}
	if now.Sub(r.lastReminder) < every {
		return model.Event{}, false
	}
	r.lastReminder = now
	return model.Event{
		Kind: model.EventReminder, At: now, Monitor: ref(r.mon), Status: model.StatusDown, Previous: model.StatusDown,
		Message: r.incident.Cause, Incident: r.incident, Duration: r.incident.Duration(now),
	}, true
}

// certWarning raises at most one warning per day while a certificate is
// inside the warning window, and re-arms once it has been renewed.
func (e *Engine) certWarning(ctx context.Context, r *runner, cfg *config.Config, now time.Time) (model.Event, bool) {
	if r.certExpiry.IsZero() {
		return model.Event{}, false
	}
	left := r.certExpiry.Sub(now)
	warn := cfg.CertExpiryWarnFor(r.mon)
	if left >= warn {
		if !r.certWarned.IsZero() {
			r.certWarned = time.Time{}
			_ = e.store.SetCertWarned(ctx, r.mon.ID, time.Time{})
		}
		return model.Event{}, false
	}
	if !r.certWarned.IsZero() && now.Sub(r.certWarned) < 24*time.Hour {
		return model.Event{}, false
	}
	r.certWarned = now
	if err := e.store.SetCertWarned(ctx, r.mon.ID, now); err != nil {
		e.log.Error("store cert warning", "monitor", r.mon.ID, "err", err)
	}
	return model.Event{
		Kind: model.EventCert, At: now, Monitor: ref(r.mon), Status: r.tracker.status, Previous: r.tracker.status,
		CertExpiry: r.certExpiry, Duration: left,
	}, true
}

func (e *Engine) publishStatus(r *runner, cfg *config.Config, prev model.Status) {
	st := e.snapshot(r, cfg)
	e.hub.Publish(model.Event{Kind: model.EventStatus, At: st.Since, Monitor: ref(st.Monitor), Status: st.Status, Previous: prev, Incident: st.Incident})
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// housekeeping marks liveness, rolls up latency aggregates and prunes old data.
func (e *Engine) housekeeping(ctx context.Context) {
	const historyKeep = 400 * 24 * time.Hour
	var lastPrune, lastRollup time.Time
	start := e.clock.Now()
	for {
		timer := e.clock.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Final liveness mark, so the next start knows exactly when
			// monitoring stopped.
			_ = e.store.Touch(context.Background(), e.clock.Now())
			return
		case <-timer.C():
		}
		now := e.clock.Now()
		if err := e.store.Touch(ctx, now); err != nil && ctx.Err() == nil {
			e.log.Error("liveness mark", "err", err)
		}
		if now.Sub(lastRollup) < 5*time.Minute || now.Sub(start) < time.Minute {
			continue
		}
		lastRollup = now
		if err := e.store.Rollup(ctx, now); err != nil && ctx.Err() == nil {
			e.log.Error("latency rollup", "err", err)
		}
		if now.Sub(lastPrune) >= time.Hour {
			lastPrune = now
			retention := e.Config().Storage.Retention.D()
			n, err := e.store.Prune(ctx, now, retention, historyKeep)
			if err != nil && ctx.Err() == nil {
				e.log.Error("prune", "err", err)
			} else if n > 0 {
				e.log.Info("pruned old checks", "deleted", n)
			}
		}
	}
}
