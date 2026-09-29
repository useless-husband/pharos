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
	ctx     context.Context
	cancel  context.CancelFunc
	closing bool
}

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
		newSender: opts.NewSender, sem: make(chan struct{}, 8), ctx: ctx, cancel: cancel, tails: map[string]chan struct{}{}}
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
		d.enqueue(ev, n.Name, d.senders[n.Name])
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
func (d *Dispatcher) enqueue(ev model.Event, notifier string, s Sender) {
	cfg := d.cfg
	msg := Render(ev, cfg.StatusPage.Language, cfg.StatusPage.Location(), cfg.Server.BaseURL)
	var incidentID int64
	if ev.Incident != nil {
		incidentID = ev.Incident.ID
	}
	key := ev.Monitor.ID + "\x00" + notifier
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
		d.deliver(msg, notifier, s, incidentID)
	}()
}

func (d *Dispatcher) deliver(msg Message, notifier string, s Sender, incidentID int64) {
	ctx := d.ctx
	var id int64
	if d.log != nil {
		var err error
		id, err = d.log.AddNotification(context.Background(), store.NotificationLog{
			Created: d.clock.Now(), MonitorID: msg.Event.Monitor.ID, IncidentID: incidentID,
			Event: string(msg.Event.Kind), Notifier: notifier,
		})
		if err != nil {
			d.logger.Error("record notification", "err", err)
		}
	}
	record := func(state string, attempts int, lastErr string, delivered time.Time) {
		if d.log != nil && id != 0 {
			if err := d.log.UpdateNotification(context.Background(), id, state, attempts, lastErr, delivered); err != nil {
				d.logger.Error("record notification", "err", err)
			}
		}
	}

	var lastErr error
	for attempt := 0; attempt <= len(d.backoff); attempt++ {
		if attempt > 0 {
			t := d.clock.NewTimer(d.backoff[attempt-1])
			select {
			case <-ctx.Done():
				t.Stop()
				record(store.NotifyFailed, attempt, "interrupted by shutdown: "+errString(lastErr), time.Time{})
				return
			case <-t.C():
			}
		}
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			record(store.NotifyFailed, attempt, "interrupted by shutdown", time.Time{})
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		lastErr = s.Send(sendCtx, msg)
		cancel()
		<-d.sem
		if lastErr == nil {
			record(store.NotifyDelivered, attempt+1, "", d.clock.Now())
			d.logger.Info("notification delivered", "notifier", notifier, "event", msg.Event.Kind, "monitor", msg.Event.Monitor.ID)
			return
		}
		var perm *PermanentError
		permanent := errors.As(lastErr, &perm)
		d.logger.Warn("notification failed", "notifier", notifier, "event", msg.Event.Kind, "attempt", attempt+1, "permanent", permanent, "err", lastErr)
		if permanent {
			record(store.NotifyFailed, attempt+1, lastErr.Error(), time.Time{})
			return
		}
		record(store.NotifyPending, attempt+1, lastErr.Error(), time.Time{})
	}
	record(store.NotifyFailed, len(d.backoff)+1, errString(lastErr), time.Time{})
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
	d.closing = true
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
