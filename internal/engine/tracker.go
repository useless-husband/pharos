package engine

import (
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

// tracker turns a stream of individual check results into confirmed status
// changes. A single failed check is noise; confirmed changes follow these
// rules, with N = confirm.down and M = confirm.up:
//
//   - down:     N failed checks with no healthy (up) check between them.
//     Slow checks in between neither count nor reset the streak, so a
//     service alternating between failing and slow is still reported down.
//   - degraded: N consecutive unhealthy checks (slow or failed) from up,
//     when there are not yet enough failures to call it down.
//   - recovery: M consecutive checks that did not fail, from down; the new
//     status is the latest result (up or degraded).
//   - degraded → up: M consecutive up checks.
//
// The first healthy result after start-up is accepted immediately.
//
// A change is dated from the first check of the streak that confirmed it,
// so an outage is dated from when it began, not from when it was confirmed.
type tracker struct {
	status model.Status
	since  time.Time

	fails     int // failed checks since the last up check
	failStart time.Time
	failMsg   string

	unhealthy      int // consecutive non-up checks (while up)
	unhealthyStart time.Time
	unhealthyMsg   string

	healthy      int // consecutive non-failed checks (while down) or up checks (while degraded)
	healthyStart time.Time
}

// change is a confirmed status transition.
type change struct {
	from, to model.Status
	at       time.Time
	cause    string
}

// reset forgets any pending streak and sets a new baseline status.
func (t *tracker) reset(status model.Status, since time.Time) {
	*t = tracker{status: status, since: since}
}

// pending reports whether a change is building up, which is when the engine
// switches to the faster retry interval.
func (t *tracker) pending() bool {
	switch t.status {
	case model.StatusUp:
		return t.fails > 0 || t.unhealthy > 0
	case model.StatusDegraded:
		return t.fails > 0 || t.healthy > 0
	case model.StatusDown:
		return t.healthy > 0
	}
	return t.fails > 0
}

// observe feeds one check (up, degraded or down) and returns a change when
// the check confirms one.
func (t *tracker) observe(c model.Check, th config.Confirm) (change, bool) {
	switch c.Status {
	case model.StatusUp:
		t.fails, t.unhealthy = 0, 0
	case model.StatusDown:
		if t.fails == 0 {
			t.failStart, t.failMsg = c.At, c.Message
		}
		t.fails++
		t.bumpUnhealthy(c)
	case model.StatusDegraded:
		t.bumpUnhealthy(c)
	}

	switch t.status {
	case model.StatusUnknown:
		switch {
		case c.Status == model.StatusDown:
			if t.fails >= th.Down {
				return t.commit(model.StatusDown, t.failStart, t.failMsg, c.At), true
			}
		default: // a healthy or slow first result is accepted at once
			return t.commit(c.Status, c.At, c.Message, c.At), true
		}

	case model.StatusUp:
		if t.fails >= th.Down {
			return t.commit(model.StatusDown, t.failStart, t.failMsg, c.At), true
		}
		if t.unhealthy >= th.Down {
			return t.commit(model.StatusDegraded, t.unhealthyStart, t.unhealthyMsg, c.At), true
		}

	case model.StatusDegraded:
		if t.fails >= th.Down {
			return t.commit(model.StatusDown, t.failStart, t.failMsg, c.At), true
		}
		if c.Status == model.StatusUp {
			t.bumpHealthy(c)
			if t.healthy >= th.Up {
				return t.commit(model.StatusUp, t.healthyStart, "", c.At), true
			}
		} else {
			t.healthy = 0
		}

	case model.StatusDown:
		if c.Status == model.StatusDown {
			t.healthy = 0
			break
		}
		t.bumpHealthy(c)
		if t.healthy >= th.Up {
			return t.commit(c.Status, t.healthyStart, "", c.At), true
		}
	}
	return change{}, false
}

func (t *tracker) bumpUnhealthy(c model.Check) {
	if t.unhealthy == 0 {
		t.unhealthyStart, t.unhealthyMsg = c.At, c.Message
	}
	t.unhealthy++
}

func (t *tracker) bumpHealthy(c model.Check) {
	if t.healthy == 0 {
		t.healthyStart = c.At
	}
	t.healthy++
}

// commit applies a change. The effective time is the start of the streak,
// but never before the current status began: that would rewrite history.
func (t *tracker) commit(to model.Status, at time.Time, cause string, now time.Time) change {
	if at.IsZero() {
		at = now
	}
	if at.Before(t.since) {
		at = t.since
	}
	ch := change{from: t.status, to: to, at: at, cause: cause}
	next := tracker{status: to, since: at}
	if to == model.StatusDegraded {
		// No healthy check has happened: failures so far still count
		// toward an outage.
		next.fails, next.failStart, next.failMsg = t.fails, t.failStart, t.failMsg
	}
	*t = next
	return ch
}
