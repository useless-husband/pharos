package store

import (
	"context"
	"database/sql"
	"time"
)

// Notification delivery states.
const (
	NotifyPending   = "pending"
	NotifyDelivered = "delivered"
	NotifyFailed    = "failed"
)

// NotificationLog is one attempt to deliver an event to a notifier.
type NotificationLog struct {
	ID         int64     `json:"id"`
	Created    time.Time `json:"created_at"`
	MonitorID  string    `json:"monitor_id"`
	IncidentID int64     `json:"incident_id,omitempty"`
	Event      string    `json:"event"`
	Notifier   string    `json:"notifier"`
	State      string    `json:"state"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error,omitempty"`
	Delivered  time.Time `json:"delivered_at,omitzero"`
}

// AddNotification records a pending delivery and returns its id.
func (s *Store) AddNotification(ctx context.Context, n NotificationLog) (int64, error) {
	res, err := s.w.ExecContext(ctx, `INSERT INTO notifications(created, monitor_id, incident_id, event, notifier, state) VALUES (?, ?, ?, ?, ?, ?)`,
		ms(n.Created), n.MonitorID, n.IncidentID, n.Event, n.Notifier, NotifyPending)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateNotification records the outcome of a delivery attempt.
func (s *Store) UpdateNotification(ctx context.Context, id int64, state string, attempts int, lastErr string, delivered time.Time) error {
	_, err := s.w.ExecContext(ctx, `UPDATE notifications SET state = ?, attempts = ?, last_error = ?, delivered = ? WHERE id = ?`,
		state, attempts, lastErr, nullMS(delivered), id)
	return err
}

// Notifications returns the most recent delivery records, newest first.
func (s *Store) Notifications(ctx context.Context, limit int) ([]NotificationLog, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, created, monitor_id, incident_id, event, notifier, state, attempts, last_error, delivered FROM notifications ORDER BY created DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationLog
	for rows.Next() {
		var n NotificationLog
		var created int64
		var delivered sql.NullInt64
		if err := rows.Scan(&n.ID, &created, &n.MonitorID, &n.IncidentID, &n.Event, &n.Notifier, &n.State, &n.Attempts, &n.LastError, &delivered); err != nil {
			return nil, err
		}
		n.Created, n.Delivered = fromMS(created), fromNullMS(delivered)
		out = append(out, n)
	}
	return out, rows.Err()
}

// FailStaleNotifications marks deliveries that were pending when the process
// stopped as failed, so the log never shows them as in flight forever.
func (s *Store) FailStaleNotifications(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, `UPDATE notifications SET state = ?, last_error = 'interrupted by shutdown' WHERE state = ?`, NotifyFailed, NotifyPending)
	return err
}
