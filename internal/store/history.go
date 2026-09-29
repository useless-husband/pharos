package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/model"
)

// MonitorState is what the store remembers about a monitor across restarts.
type MonitorState struct {
	ID         string
	FirstSeen  time.Time
	Paused     bool
	CertWarned time.Time
	// Status and Since describe the open status period, if any.
	Status model.Status
	Since  time.Time
	// Incident is the open incident, if any.
	Incident *model.Incident
	// LastCheck is the most recent stored check, if any.
	LastCheck *model.Check
}

// EnsureMonitor records a monitor's existence; first_seen is kept on later calls.
func (s *Store) EnsureMonitor(ctx context.Context, id string, now time.Time) error {
	_, err := s.w.ExecContext(ctx, `INSERT OR IGNORE INTO monitors(id, first_seen) VALUES (?, ?)`, id, ms(now))
	return err
}

// SetPaused persists the operator's pause switch.
func (s *Store) SetPaused(ctx context.Context, id string, paused bool) error {
	_, err := s.w.ExecContext(ctx, `UPDATE monitors SET paused = ? WHERE id = ?`, paused, id)
	return err
}

// SetCertWarned records when a certificate warning was last sent.
func (s *Store) SetCertWarned(ctx context.Context, id string, at time.Time) error {
	_, err := s.w.ExecContext(ctx, `UPDATE monitors SET cert_warned = ? WHERE id = ?`, ms(at), id)
	return err
}

// LoadStates returns the persisted state of every known monitor.
func (s *Store) LoadStates(ctx context.Context) (map[string]*MonitorState, error) {
	out := map[string]*MonitorState{}
	rows, err := s.w.QueryContext(ctx, `SELECT id, first_seen, paused, cert_warned FROM monitors`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st MonitorState
		var first, warned int64
		if err := rows.Scan(&st.ID, &first, &st.Paused, &warned); err != nil {
			rows.Close()
			return nil, err
		}
		st.FirstSeen, st.CertWarned = fromMS(first), fromMS(warned)
		out[st.ID] = &st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.w.QueryContext(ctx, `SELECT monitor_id, status, started FROM periods WHERE ended IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var status int
		var started int64
		if err := rows.Scan(&id, &status, &started); err != nil {
			rows.Close()
			return nil, err
		}
		if st := out[id]; st != nil {
			st.Status, st.Since = model.Status(status), fromMS(started)
		}
	}
	rows.Close()

	incs, err := s.Incidents(ctx, IncidentQuery{OpenOnly: true})
	if err != nil {
		return nil, err
	}
	for i := range incs {
		if st := out[incs[i].MonitorID]; st != nil {
			st.Incident = &incs[i]
		}
	}
	for id, st := range out {
		c, err := s.LastCheck(ctx, id)
		if err != nil {
			return nil, err
		}
		st.LastCheck = c
	}
	return out, nil
}

// Transition records a confirmed status change atomically: it closes the
// open period, opens the next one, and opens or closes an incident.
type Transition struct {
	MonitorID string
	To        model.Status
	At        time.Time
	// Cause is recorded on the incident opened when To is Down.
	Cause string
	// Resolution is recorded on the open incident when leaving Down.
	Resolution string
}

// ApplyTransition persists t. It returns the incident that was opened or
// closed by the transition, if any.
func (s *Store) ApplyTransition(ctx context.Context, t Transition) (*model.Incident, error) {
	var inc *model.Incident
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		at := ms(t.At)
		// A transition can never be dated before the open period began
		// (clock skew, a check that started before a pause): clamp it, so
		// periods never overlap.
		var openStart sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT started FROM periods WHERE monitor_id = ? AND ended IS NULL`, t.MonitorID).Scan(&openStart); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if openStart.Valid && at < openStart.Int64 {
			at = openStart.Int64
		}
		if _, err := tx.ExecContext(ctx, `UPDATE periods SET ended = ? WHERE monitor_id = ? AND ended IS NULL`, at, t.MonitorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO periods(monitor_id, status, started) VALUES (?, ?, ?)`, t.MonitorID, int(t.To), at); err != nil {
			return err
		}
		open, err := openIncident(ctx, tx, t.MonitorID)
		if err != nil {
			return err
		}
		switch {
		case t.To == model.StatusDown && open == nil:
			res, err := tx.ExecContext(ctx, `INSERT INTO incidents(monitor_id, started, cause) VALUES (?, ?, ?)`, t.MonitorID, at, t.Cause)
			if err != nil {
				return err
			}
			id, _ := res.LastInsertId()
			inc = &model.Incident{ID: id, MonitorID: t.MonitorID, Started: fromMS(at), Cause: t.Cause}
		case t.To != model.StatusDown && open != nil:
			end := max(at, ms(open.Started))
			if _, err := tx.ExecContext(ctx, `UPDATE incidents SET ended = ?, resolution = ? WHERE id = ?`, end, t.Resolution, open.ID); err != nil {
				return err
			}
			open.Ended, open.Resolution = fromMS(end), t.Resolution
			inc = open
		}
		return nil
	})
	return inc, err
}

func openIncident(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*model.Incident, error) {
	var inc model.Incident
	var started int64
	err := q.QueryRowContext(ctx, `SELECT id, monitor_id, started, cause FROM incidents WHERE monitor_id = ? AND ended IS NULL`, id).
		Scan(&inc.ID, &inc.MonitorID, &started, &inc.Cause)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inc.Started = fromMS(started)
	return &inc, nil
}

// IncidentQuery filters Incidents.
type IncidentQuery struct {
	MonitorIDs []string // empty: all monitors
	Since      time.Time
	OpenOnly   bool
	Limit      int
}

// Incidents returns incidents newest first. An incident is included if it
// was still open at Since or started after it.
func (s *Store) Incidents(ctx context.Context, q IncidentQuery) ([]model.Incident, error) {
	var where []string
	var args []any
	if q.OpenOnly {
		where = append(where, "ended IS NULL")
	}
	if !q.Since.IsZero() {
		where = append(where, "(ended IS NULL OR ended >= ?)")
		args = append(args, ms(q.Since))
	}
	if len(q.MonitorIDs) > 0 {
		where = append(where, "monitor_id IN ("+placeholders(len(q.MonitorIDs))+")")
		for _, id := range q.MonitorIDs {
			args = append(args, id)
		}
	}
	query := `SELECT id, monitor_id, started, ended, cause, resolution FROM incidents`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY started DESC, id DESC"
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Incident
	for rows.Next() {
		var inc model.Incident
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&inc.ID, &inc.MonitorID, &started, &ended, &inc.Cause, &inc.Resolution); err != nil {
			return nil, err
		}
		inc.Started, inc.Ended = fromMS(started), fromNullMS(ended)
		out = append(out, inc)
	}
	return out, rows.Err()
}

// Incident returns a single incident by id.
func (s *Store) Incident(ctx context.Context, id int64) (*model.Incident, error) {
	var inc model.Incident
	var started int64
	var ended sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT id, monitor_id, started, ended, cause, resolution FROM incidents WHERE id = ?`, id).
		Scan(&inc.ID, &inc.MonitorID, &started, &ended, &inc.Cause, &inc.Resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inc.Started, inc.Ended = fromMS(started), fromNullMS(ended)
	return &inc, nil
}

// Periods returns status periods overlapping [from, to), grouped by monitor
// and ordered by start time.
func (s *Store) Periods(ctx context.Context, from, to time.Time, ids ...string) (map[string][]model.Period, error) {
	query := `SELECT monitor_id, status, started, ended FROM periods WHERE started < ? AND (ended IS NULL OR ended > ?)`
	args := []any{ms(to), ms(from)}
	if len(ids) > 0 {
		query += " AND monitor_id IN (" + placeholders(len(ids)) + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += " ORDER BY monitor_id, started"
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.Period{}
	for rows.Next() {
		var p model.Period
		var status int
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&p.MonitorID, &status, &started, &ended); err != nil {
			return nil, err
		}
		p.Status, p.Start, p.End = model.Status(status), fromMS(started), fromNullMS(ended)
		out[p.MonitorID] = append(out[p.MonitorID], p)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
