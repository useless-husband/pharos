// Package model defines the core types shared by every Pharos subsystem:
// statuses, check results, incidents and status periods.
package model

import (
	"fmt"
	"time"
)

// Status is the state of a monitor, or the outcome of a single check.
//
// Checks only ever produce Up, Degraded or Down. The remaining values describe
// a monitor as a whole: Unknown before the first result is confirmed,
// Maintenance while a maintenance window is active, Paused when an operator
// has paused it.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusUp
	StatusDegraded
	StatusDown
	StatusMaintenance
	StatusPaused
)

var statusNames = [...]string{
	StatusUnknown:     "unknown",
	StatusUp:          "up",
	StatusDegraded:    "degraded",
	StatusDown:        "down",
	StatusMaintenance: "maintenance",
	StatusPaused:      "paused",
}

func (s Status) String() string {
	if int(s) < len(statusNames) {
		return statusNames[s]
	}
	return fmt.Sprintf("status(%d)", s)
}

// ParseStatus is the inverse of String.
func ParseStatus(v string) (Status, bool) {
	for i, n := range statusNames {
		if n == v {
			return Status(i), true
		}
	}
	return StatusUnknown, false
}

func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s *Status) UnmarshalText(b []byte) error {
	v, ok := ParseStatus(string(b))
	if !ok {
		return fmt.Errorf("unknown status %q", b)
	}
	*s = v
	return nil
}

// Available reports whether the status counts as "serving traffic" for
// availability purposes. Degraded is available: slow is not down.
func (s Status) Available() bool { return s == StatusUp || s == StatusDegraded }

// Counted reports whether time spent in this status contributes to the
// availability denominator. Maintenance, paused and unknown time is excluded.
func (s Status) Counted() bool {
	return s == StatusUp || s == StatusDegraded || s == StatusDown
}

// Timing is the phase breakdown of a network check. Phases that did not
// happen (for example TLS on a plain HTTP request) are zero.
type Timing struct {
	DNS       time.Duration `json:"dns"`
	Connect   time.Duration `json:"connect"`
	TLS       time.Duration `json:"tls"`
	FirstByte time.Duration `json:"first_byte"`
}

// Check is the result of probing a monitor once.
type Check struct {
	MonitorID string
	At        time.Time
	Status    Status // Up, Degraded or Down
	Latency   time.Duration
	// Message explains a Down or Degraded result in one line, e.g.
	// "HTTP 503 Service Unavailable" or "response took 2.4s (limit 2s)".
	Message string
	Timing  *Timing
	// CertExpiry is the leaf certificate's NotAfter, when a TLS handshake happened.
	CertExpiry time.Time
	// Maintenance is set when the check ran inside a maintenance window.
	// Such checks are stored but never change the monitor's state.
	Maintenance bool
}

// Incident is a confirmed outage: a period during which a monitor was Down.
type Incident struct {
	ID        int64     `json:"id"`
	MonitorID string    `json:"monitor_id"`
	Started   time.Time `json:"started_at"`
	Ended     time.Time `json:"ended_at,omitzero"`
	// Cause is the failure message of the check that opened the incident.
	Cause string `json:"cause"`
	// Resolution describes how the incident ended, e.g. "recovered" or
	// "maintenance started".
	Resolution string `json:"resolution,omitempty"`
}

// Ongoing reports whether the incident has not ended yet.
func (i Incident) Ongoing() bool { return i.Ended.IsZero() }

// Duration returns how long the incident lasted, measuring ongoing
// incidents up to now.
func (i Incident) Duration(now time.Time) time.Duration {
	end := i.Ended
	if end.IsZero() {
		end = now
	}
	if end.Before(i.Started) {
		return 0
	}
	return end.Sub(i.Started)
}

// Period is a span of time a monitor spent in one confirmed status.
// Availability is computed from periods, not from individual checks, so it
// reflects confirmed state and excludes maintenance and paused time exactly.
type Period struct {
	MonitorID string
	Status    Status
	Start     time.Time
	End       time.Time // zero while the period is still open
}

// Overlap returns how much of the period falls inside [from, to), treating
// an open period as ending at now.
func (p Period) Overlap(from, to, now time.Time) time.Duration {
	end := p.End
	if end.IsZero() {
		end = now
	}
	start := p.Start
	if start.Before(from) {
		start = from
	}
	if end.After(to) {
		end = to
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}
