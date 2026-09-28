package engine

import (
	"sync"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

// MonitorState is a point-in-time view of one monitor.
type MonitorState struct {
	Monitor     config.Monitor
	Status      model.Status
	Since       time.Time
	LastCheck   *model.Check
	Incident    *model.Incident
	CertExpiry  time.Time
	FirstSeen   time.Time
	NextCheck   time.Time
	Maintenance *Window
	// PushToken is set for push monitors.
	PushToken string
}

// Snapshot returns every monitor's state in configuration order.
func (e *Engine) Snapshot() []MonitorState {
	e.mu.RLock()
	cfg := e.cfg
	runners := make([]*runner, 0, len(e.order))
	for _, id := range e.order {
		runners = append(runners, e.runners[id])
	}
	e.mu.RUnlock()
	out := make([]MonitorState, 0, len(runners))
	for _, r := range runners {
		out = append(out, e.snapshot(r, cfg))
	}
	return out
}

// State returns one monitor's state.
func (e *Engine) State(id string) (MonitorState, bool) {
	r, cfg := e.runner(id)
	if r == nil {
		return MonitorState{}, false
	}
	return e.snapshot(r, cfg), true
}

func (e *Engine) snapshot(r *runner, _ *config.Config) MonitorState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := MonitorState{
		Monitor:     r.mon,
		Status:      r.tracker.status,
		Since:       r.tracker.since,
		CertExpiry:  r.certExpiry,
		FirstSeen:   r.firstSeen,
		NextCheck:   r.nextCheck,
		Maintenance: r.maint,
	}
	if r.paused {
		st.Status = model.StatusPaused
	}
	if r.lastCheck != nil {
		c := *r.lastCheck
		st.LastCheck = &c
	}
	if r.incident != nil {
		inc := *r.incident
		st.Incident = &inc
	}
	if r.mon.Type == config.TypePush {
		st.PushToken = e.PushToken(r.mon)
	}
	return st
}

// Hub fans events out to live subscribers (the dashboard's event stream).
// Slow subscribers miss events rather than slowing the engine down.
type Hub struct {
	mu   sync.Mutex
	subs map[chan model.Event]struct{}
}

func newHub() *Hub { return &Hub{subs: map[chan model.Event]struct{}{}} }

// Subscribe returns a channel of events and a function to unsubscribe.
func (h *Hub) Subscribe() (<-chan model.Event, func()) {
	ch := make(chan model.Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish delivers ev to every subscriber that has room for it.
func (h *Hub) Publish(ev model.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
