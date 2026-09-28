// Package clock abstracts time so schedulers can be tested deterministically.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the subset of the time package the engine uses.
type Clock interface {
	Now() time.Time
	// NewTimer returns a timer that fires once after d.
	NewTimer(d time.Duration) Timer
}

// Timer is a stoppable one-shot timer.
type Timer interface {
	C() <-chan time.Time
	// Stop prevents the timer from firing. Always stop timers you stop
	// waiting on, so fake clocks know nobody is waiting any more.
	Stop()
}

// Real is the system clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop()               { r.t.Stop() }

// Fake is a manually advanced clock for tests.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
	changed chan struct{}
	nextID  uint64
}

type waiter struct {
	at time.Time
	ch chan time.Time
	id uint64
}

type fakeTimer struct {
	f  *Fake
	ch chan time.Time
	id uint64
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	for i, w := range t.f.waiters {
		if w.id == t.id {
			t.f.waiters = append(t.f.waiters[:i], t.f.waiters[i+1:]...)
			return
		}
	}
}

// NewFake returns a fake clock set to start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, changed: make(chan struct{})}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	t := &fakeTimer{f: f, ch: make(chan time.Time, 1), id: f.nextID}
	if d <= 0 {
		t.ch <- f.now
		return t
	}
	f.waiters = append(f.waiters, waiter{at: f.now.Add(d), ch: t.ch, id: t.id})
	close(f.changed)
	f.changed = make(chan struct{})
	return t
}

// Advance moves the clock forward and fires every timer that became due,
// in deadline order.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	sort.Slice(f.waiters, func(i, j int) bool { return f.waiters[i].at.Before(f.waiters[j].at) })
	kept := f.waiters[:0]
	var due []waiter
	for _, w := range f.waiters {
		if !w.at.After(now) {
			due = append(due, w)
		} else {
			kept = append(kept, w)
		}
	}
	f.waiters = kept
	f.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

// Waiters returns how many timers are pending.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntil waits until at least n timers are pending, so a test can be
// sure every goroutine has gone back to sleep before advancing.
func (f *Fake) BlockUntil(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		if len(f.waiters) >= n {
			f.mu.Unlock()
			return true
		}
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			return false
		}
	}
}
