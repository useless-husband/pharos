package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/useless-husband/pharos/internal/clock"
	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
)

// DefaultBackoff is the wait before each retry of a failed delivery.
var DefaultBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Log records delivery attempts; *store.Store implements it.
type Log interface {
	AddNotification(ctx context.Context, n store.NotificationLog) (int64, error)
	UpdateNotification(ctx context.Context, id int64, state string, attempts int, lastErr string, delivered time.Time) error
}

// Dispatcher routes events to notifiers and delivers them in the
// background. Notify never blocks the caller.
type Dispatcher struct {
	log       Log
	logger    *slog.Logger
	clock     clock.Clock
	backoff   []time.Duration
	newSender func(config.Notifier) (Sender, error)

	mu      sync.RWMutex
	cfg     *config.Config
	senders map[string]Sender

	sem chan struct{}
	wg  sync.WaitGroup
	// tails chains deliveries per monitor and notifier, so a DOWN that is
	// being retried is never overtaken by the RECOVERED that follows it.
	tails   map[string]chan struct{}
	tailsMu sync.Mutex
	// groups collect the events of notifiers with a group interval; each
	// sends at most one message per interval.
	groups   map[string]*group
	groupsMu sync.Mutex
	flush    chan struct{} // closed by Close: send pending groups now
	ctx      context.Context
	cancel   context.CancelFunc
	closing  bool
}

// queued is an event waiting for delivery, with its delivery log entry
// (zero if it could not be recorded).
type queued struct {
	ev    model.Event
	logID int64
}

// group is the pending events of one notifier and when it last sent.
type group struct {
	pending  []queued
	running  bool // a goroutine is sending this group
	lastSent time.Time
}

// maxRateLimitWaits bounds how often one delivery waits out a rate limit
// before it is given up.
const maxRateLimitWaits = 20

// Options configure a Dispatcher.
type Options struct {
	Log     Log
	Logger  *slog.Logger
	Clock   clock.Clock
	Backoff []time.Duration
	// NewSender builds senders; tests replace it.
	NewSender func(config.Notifier) (Sender, error)
}

// NewDispatcher creates a dispatcher for cfg.
func NewDispatcher(cfg *config.Config, opts Options) (*Dispatcher, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Backoff == nil {
		opts.Backoff = DefaultBackoff
	}
	if opts.NewSender == nil {
		opts.NewSender = NewSender
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{log: opts.Log, logger: opts.Logger, clock: opts.Clock, backoff: opts.Backoff,
		newSender: opts.NewSender, sem: make(chan struct{}, 8), ctx: ctx, cancel: cancel, tails: map[string]chan struct{}{},
		groups: map[string]*group{}, flush: make(chan struct{})}
	if err := d.Reload(cfg); err != nil {
		cancel()
		return nil, err
	}
	return d, nil
}

// Reload swaps in a new configuration. Deliveries already in flight finish
// with the sender they started with.
func (d *Dispatcher) Reload(cfg *config.Config) error {
	senders := map[string]Sender{}
	for _, n := range cfg.Notifiers {
		s, err := d.newSender(n)
		if err != nil {
			return fmt.Errorf("notifier %s: %w", n.Name, err)
		}
		senders[n.Name] = s
	}
	d.mu.Lock()
	d.cfg, d.senders = cfg, senders
	d.mu.Unlock()
	return nil
}

// Notify routes an event to the notifiers of its monitor.
func (d *Dispatcher) Notify(ev model.Event) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closing {
		return
	}
	m, ok := d.cfg.MonitorByID(ev.Monitor.ID)
	if !ok {
		return
	}
	for _, name := range d.cfg.NotifiersFor(m) {
		n, ok := d.cfg.NotifierByName(name)
		if !ok || !n.Wants(string(ev.Kind)) {
			continue
		}
		// Record the delivery as pending now, so that alerts waiting their
		// turn show in the delivery log and are accounted for if Pharos stops.
		q := queued{ev: ev, logID: d.logPending(ev, n.Name)}
		if n.Type == config.NotifyWebhook {
			d.enqueue(q, n.Name, d.senders[n.Name])
		} else {
			d.addToGroup(q, n.Name)
		}
	}
}

func (d *Dispatcher) logPending(ev model.Event, notifier string) int64 {
	if d.log == nil {
		return 0
	}
	var incidentID int64
	if ev.Incident != nil {
		incidentID = ev.Incident.ID
	}
	id, err := d.log.AddNotification(context.Background(), store.NotificationLog{
		Created: d.clock.Now(), MonitorID: ev.Monitor.ID, IncidentID: incidentID,
		Event: string(ev.Kind), Notifier: notifier,
	})
	if err != nil {
		d.logger.Error("record notification", "err", err)
	}
	return id
}

// addToGroup queues an event for a chat, push or email notifier. These
// send one message at a time, so that a rate-limited channel is told to
// slow down once rather than refusing a crowd of parallel retries. The
// first event after a quiet group interval is sent at once; events that
// follow within it wait and go out together (with a zero interval, one by
// one). Caller holds d.mu (read).
func (d *Dispatcher) addToGroup(q queued, notifier string) {
	d.groupsMu.Lock()
	defer d.groupsMu.Unlock()
	g := d.groups[notifier]
	if g == nil {
		g = &group{}
		d.groups[notifier] = g
	}
	g.pending = append(g.pending, q)
	if !g.running {
		g.running = true
		d.wg.Add(1)
		go d.sendGroup(notifier, g)
	}
}

// sendGroup sends a notifier's pending events, one message per group
// interval, until none are left. Messages go out one after another, so
// they arrive in order and a rate-limited channel is never flooded; events
// that arrive while a message is being retried join the next one, unless
// grouping is off.
func (d *Dispatcher) sendGroup(notifier string, g *group) {
	defer d.wg.Done()
	for {
		d.mu.RLock()
		cfg, s := d.cfg, d.senders[notifier]
		n, known := cfg.NotifierByName(notifier)
		d.mu.RUnlock()

		d.groupsMu.Lock()
		if len(g.pending) == 0 {
			g.running = false
			d.groupsMu.Unlock()
			return
		}
		wait := g.lastSent.Add(n.Grouping()).Sub(d.clock.Now())
		d.groupsMu.Unlock()
		if wait > 0 {
			t := d.clock.NewTimer(wait)
			select {
			case <-t.C():
			case <-d.flush:
				t.Stop()
			case <-d.ctx.Done():
				t.Stop()
			}
		}

		d.groupsMu.Lock()
		var batch []queued
		if n.Grouping() == 0 {
			batch, g.pending = g.pending[:1:1], g.pending[1:]
		} else {
			batch, g.pending = g.pending, nil
		}
		g.lastSent = d.clock.Now()
		d.groupsMu.Unlock()
		if !known || s == nil {
			d.logger.Warn("notifier removed; dropping its pending notifications", "notifier", notifier, "events", len(batch))
			d.record(batch, store.NotifyFailed, 0, "notifier removed from the configuration", time.Time{})
			continue
		}
		evs := make([]model.Event, len(batch))
		for i, q := range batch {
			evs[i] = q.ev
		}
		msg := RenderGroup(evs, cfg.StatusPage.Language, cfg.StatusPage.Location(), cfg.Server.BaseURL)
		d.deliver(msg, batch, notifier, s)
	}
}

// Test sends a test event to one notifier and waits for the result,
// without retries.
func (d *Dispatcher) Test(ctx context.Context, name string) error {
	d.mu.RLock()
	s, ok := d.senders[name]
	cfg := d.cfg
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no notifier named %q", name)
	}
	ev := model.Event{Kind: model.EventTest, At: d.clock.Now(), Message: name}
	msg := Render(ev, cfg.StatusPage.Language, cfg.StatusPage.Location(), cfg.Server.BaseURL)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.Send(ctx, msg)
}

// enqueue starts a delivery. Caller holds d.mu (read).
func (d *Dispatcher) enqueue(q queued, notifier string, s Sender) {
	cfg := d.cfg
	msg := Render(q.ev, cfg.StatusPage.Language, cfg.StatusPage.Location(), cfg.Server.BaseURL)
	key := q.ev.Monitor.ID + "\x00" + notifier
	done := make(chan struct{})
	d.tailsMu.Lock()
	prev := d.tails[key]
	d.tails[key] = done
	d.tailsMu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			close(done)
			d.tailsMu.Lock()
			if d.tails[key] == done {
				delete(d.tails, key)
			}
			d.tailsMu.Unlock()
		}()
		if prev != nil {
			select {
			case <-prev:
			case <-d.ctx.Done():
			}
		}
		d.deliver(msg, []queued{q}, notifier, s)
	}()
}

// record updates the delivery log entries of items.
func (d *Dispatcher) record(items []queued, state string, attempts int, lastErr string, delivered time.Time) {
	if d.log == nil {
		return
	}
	for _, q := range items {
		if q.logID == 0 {
			continue
		}
		if err := d.log.UpdateNotification(context.Background(), q.logID, state, attempts, lastErr, delivered); err != nil {
			d.logger.Error("record notification", "err", err)
		}
	}
}

// deliver sends msg, which reports the events of items, with retries, and
// keeps their delivery log entries up to date.
func (d *Dispatcher) deliver(msg Message, items []queued, notifier string, s Sender) {
	ctx := d.ctx
	record := func(state string, attempts int, lastErr string, delivered time.Time) {
		d.record(items, state, attempts, lastErr, delivered)
	}

	var lastErr error
	var wait time.Duration
	attempts, failures, limited := 0, 0, 0
	for {
		if wait > 0 {
			t := d.clock.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				record(store.NotifyFailed, attempts, "interrupted by shutdown: "+errString(lastErr), time.Time{})
				return
			case <-t.C():
			}
		}
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			record(store.NotifyFailed, attempts, "interrupted by shutdown", time.Time{})
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		lastErr = s.Send(sendCtx, msg)
		cancel()
		<-d.sem
		attempts++
		if lastErr == nil {
			record(store.NotifyDelivered, attempts, "", d.clock.Now())
			d.logger.Info("notification delivered", "notifier", notifier, "event", msg.Event.Kind, "monitor", msg.Event.Monitor.ID, "events", len(items))
			return
		}
		var perm *PermanentError
		var limit *RateLimitError
		switch {
		case errors.As(lastErr, &limit) && limited < maxRateLimitWaits:
			// Being told to slow down is not a failure of the message:
			// wait as long as asked, without using up a retry.
			limited++
			wait = limit.After
			if wait <= 0 {
				wait = DefaultBackoff[0]
			}
			d.logger.Warn("notification rate limited", "notifier", notifier, "retry_in", wait)
			record(store.NotifyPending, attempts, lastErr.Error(), time.Time{})
			continue
		case errors.As(lastErr, &perm):
			d.logger.Warn("notification failed", "notifier", notifier, "event", msg.Event.Kind, "attempt", attempts, "permanent", true, "err", lastErr)
			record(store.NotifyFailed, attempts, lastErr.Error(), time.Time{})
			return
		case failures == len(d.backoff):
			d.logger.Warn("notification failed", "notifier", notifier, "event", msg.Event.Kind, "attempt", attempts, "err", lastErr)
			record(store.NotifyFailed, attempts, lastErr.Error(), time.Time{})
			return
		}
		d.logger.Warn("notification failed", "notifier", notifier, "event", msg.Event.Kind, "attempt", attempts, "permanent", false, "err", lastErr)
		record(store.NotifyPending, attempts, lastErr.Error(), time.Time{})
		wait = d.backoff[failures]
		failures++
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Close stops accepting events and waits for in-flight deliveries until ctx
// ends; deliveries still waiting to retry are then abandoned.
func (d *Dispatcher) Close(ctx context.Context) {
	d.mu.Lock()
	if !d.closing {
		d.closing = true
		close(d.flush) // pending groups go out now, not after their interval
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		d.cancel()
		<-done
	}
	d.cancel()
}
