package model

import "time"

// EventKind names an event. Notification kinds match the names accepted in
// a notifier's `events` filter.
type EventKind string

const (
	EventDown     EventKind = "down"     // an incident started
	EventUp       EventKind = "up"       // an incident ended
	EventDegraded EventKind = "degraded" // performance degraded or recovered
	EventReminder EventKind = "reminder" // an incident is still ongoing
	EventCert     EventKind = "cert"     // a certificate expires soon
	EventTest     EventKind = "test"     // sent by `pharos notify-test`

	// Live-update kinds, never sent to notifiers.
	EventStatus EventKind = "status" // any confirmed status change
	EventCheck  EventKind = "check"  // a check completed
)

// MonitorRef identifies a monitor in an event.
type MonitorRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Target string `json:"target"`
}

// Event describes something that happened to a monitor.
type Event struct {
	Kind     EventKind  `json:"event"`
	At       time.Time  `json:"at"`
	Monitor  MonitorRef `json:"monitor"`
	Status   Status     `json:"status"`
	Previous Status     `json:"previous_status"`
	// Message is the failure cause, or a description of the change.
	Message  string    `json:"message,omitempty"`
	Incident *Incident `json:"incident,omitempty"`
	// Duration is the outage length for "up", time down so far for
	// "reminder", and time until expiry for "cert".
	Duration   time.Duration `json:"-"`
	CertExpiry time.Time     `json:"cert_expiry,omitzero"`
	Check      *Check        `json:"-"`
}
