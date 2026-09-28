package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; each runs once, in its own transaction.
// Never edit a migration that has shipped: append a new one.
var migrations = []string{
	// 1: initial schema. Timestamps are Unix milliseconds.
	`
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE monitors (
	id          TEXT PRIMARY KEY,
	first_seen  INTEGER NOT NULL,
	paused      INTEGER NOT NULL DEFAULT 0,
	cert_warned INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE checks (
	monitor_id  TEXT    NOT NULL,
	at          INTEGER NOT NULL,
	status      INTEGER NOT NULL,
	latency_us  INTEGER NOT NULL,
	message     TEXT    NOT NULL DEFAULT '',
	dns_us      INTEGER NOT NULL DEFAULT 0,
	connect_us  INTEGER NOT NULL DEFAULT 0,
	tls_us      INTEGER NOT NULL DEFAULT 0,
	ttfb_us     INTEGER NOT NULL DEFAULT 0,
	has_timing  INTEGER NOT NULL DEFAULT 0,
	cert_expiry INTEGER NOT NULL DEFAULT 0,
	maintenance INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX checks_by_monitor ON checks(monitor_id, at);
CREATE INDEX checks_by_time ON checks(at);

-- Confirmed status history. Exactly one open period (ended IS NULL) per
-- monitor once it has a status.
CREATE TABLE periods (
	id         INTEGER PRIMARY KEY,
	monitor_id TEXT    NOT NULL,
	status     INTEGER NOT NULL,
	started    INTEGER NOT NULL,
	ended      INTEGER
);
CREATE INDEX periods_by_monitor ON periods(monitor_id, started);
CREATE UNIQUE INDEX periods_one_open ON periods(monitor_id) WHERE ended IS NULL;

CREATE TABLE incidents (
	id         INTEGER PRIMARY KEY,
	monitor_id TEXT    NOT NULL,
	started    INTEGER NOT NULL,
	ended      INTEGER,
	cause      TEXT    NOT NULL DEFAULT '',
	resolution TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX incidents_by_monitor ON incidents(monitor_id, started);
CREATE INDEX incidents_by_time ON incidents(started);
CREATE UNIQUE INDEX incidents_one_open ON incidents(monitor_id) WHERE ended IS NULL;

CREATE TABLE latency_hourly (
	monitor_id TEXT    NOT NULL,
	hour       INTEGER NOT NULL,
	checks     INTEGER NOT NULL,
	ok         INTEGER NOT NULL,
	sum_us     INTEGER NOT NULL,
	min_us     INTEGER NOT NULL,
	max_us     INTEGER NOT NULL,
	p50_us     INTEGER NOT NULL,
	p95_us     INTEGER NOT NULL,
	PRIMARY KEY (monitor_id, hour)
) WITHOUT ROWID;

CREATE TABLE notifications (
	id          INTEGER PRIMARY KEY,
	created     INTEGER NOT NULL,
	monitor_id  TEXT    NOT NULL DEFAULT '',
	incident_id INTEGER NOT NULL DEFAULT 0,
	event       TEXT    NOT NULL,
	notifier    TEXT    NOT NULL,
	state       TEXT    NOT NULL,
	attempts    INTEGER NOT NULL DEFAULT 0,
	last_error  TEXT    NOT NULL DEFAULT '',
	delivered   INTEGER
);
CREATE INDEX notifications_by_time ON notifications(created);
`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d); upgrade Pharos", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied) VALUES (?, strftime('%s','now') * 1000)`, v); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}
