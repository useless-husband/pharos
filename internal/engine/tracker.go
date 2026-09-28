package engine

import (
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

// tracker turns a stream of individual check results into confirmed status
// changes. A single failed check is noise; Confirm.Down consecutive failures
// are an outage. The effective time of a change is the first check of the
// confirming streak, so an outage is dated from when it began, not from
// when it was confirmed.
type tracker struct {
	status model.Status
	since  time.Time

	// streak of consecutive checks that disagree with status
	candidate   model.Status
	streak      int
	streakStart time.Time
	streakMsg   string // message of the first check in the streak
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

// pending reports whether a disagreeing streak is building up, which is when
// the engine switches to the faster retry interval.
func (t *tracker) pending() bool { return t.streak > 0 }

// observe feeds one check (Up, Degraded or Down) and returns a change when
// the check confirms one.
func (t *tracker) observe(c model.Check, th config.Confirm) (change, bool) {
	cur := t.status
	var target model.Status
	var need int
	switch {
	case c.Status == model.StatusDown && cur != model.StatusDown:
		target, need = model.StatusDown, th.Down
	case c.Status != model.StatusDown && cur == model.StatusDown:
		// Recovery: any non-down result counts; the new status is the
		// latest result (up or degraded).
		target, need = c.Status, th.Up
	case c.Status != model.StatusDown && c.Status != cur:
		switch cur {
		case model.StatusUnknown:
			need = 1 // a healthy first result needs no confirmation
		case model.StatusUp:
			need = th.Down // up -> degraded is also an alert
		default:
			need = th.Up // degraded -> up
		}
		target = c.Status
	default:
		// Agrees with the current status.
		t.streak = 0
		return change{}, false
	}

	// For recovery streaks, up and degraded are the same candidate.
	sameCandidate := t.streak > 0 && (t.candidate == target ||
		(cur == model.StatusDown && t.candidate.Available() && target.Available()))
	if !sameCandidate {
		t.candidate, t.streak, t.streakStart, t.streakMsg = target, 0, c.At, c.Message
	}
	t.candidate = target
	t.streak++
	if t.streak < need {
		return change{}, false
	}
	ch := change{from: cur, to: target, at: t.streakStart, cause: t.streakMsg}
	if !ch.at.After(t.since) {
		ch.at = c.At
	}
	t.status, t.since = target, ch.at
	t.streak = 0
	return ch, true
}
