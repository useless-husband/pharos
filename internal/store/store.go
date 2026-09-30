// Package store persists check results, status history, incidents and
// notification attempts in SQLite.
//
// Writes go through a single connection; reads use a separate pool. With WAL
// journaling, dashboard queries never block the checker and vice versa.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Store is safe for concurrent use.
type Store struct {
	w *sql.DB // single writer connection
	r *sql.DB // reader pool
}

// Open opens (creating if needed) the database at path and applies
// migrations. Use ":memory:" for a throwaway database.
func Open(ctx context.Context, path string) (*Store, error) {
	pragmas := "_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)&_txlock=immediate"
	var s Store
	if path == ":memory:" {
		db, err := sql.Open("sqlite", "file::memory:?"+pragmas)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1) // an in-memory database lives in one connection
		s.w, s.r = db, db
	} else {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
		}
		dsn := "file:" + uriPath(path) + "?" + pragmas +
			"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
		w, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		w.SetMaxOpenConns(1)
		r, err := sql.Open("sqlite", dsn+"&mode=ro")
		if err != nil {
			w.Close()
			return nil, err
		}
		r.SetMaxOpenConns(4)
		s.w, s.r = w, r
	}
	if err := s.w.PingContext(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := migrate(ctx, s.w); err != nil {
		s.Close()
		return nil, err
	}
	return &s, nil
}

// uriPath turns a file path into the path part of an SQLite file: URI.
// Windows drive paths become "/C:/dir/file.db".
func uriPath(path string) string {
	p := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		p = "/" + p
	}
	return (&url.URL{Path: p}).EscapedPath()
}

// Close closes the database.
func (s *Store) Close() error {
	var err error
	if s.r != nil && s.r != s.w {
		err = s.r.Close()
	}
	if s.w != nil {
		err = errors.Join(err, s.w.Close())
	}
	return err
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

func nullMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

// Secret returns a random secret stored under key, creating it on first use.
// It is used to sign sessions and derive push tokens, so both survive restarts.
func (s *Store) Secret(ctx context.Context, key string) ([]byte, error) {
	var v string
	err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == nil {
		return hex.DecodeString(v)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if _, err := s.w.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, key, hex.EncodeToString(b)); err != nil {
		return nil, err
	}
	return s.Secret(ctx, key)
}

// SetMeta stores a small key/value setting.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Meta reads a key/value setting.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// Touch records that Pharos was running at t. After a restart, the gap
// since the last touch is known to be unobserved.
func (s *Store) Touch(ctx context.Context, t time.Time) error {
	return s.SetMeta(ctx, "alive_at", strconv.FormatInt(ms(t), 10))
}

// AliveAt returns the last Touch, or zero.
func (s *Store) AliveAt(ctx context.Context) (time.Time, error) {
	v, ok, err := s.Meta(ctx, "alive_at")
	if err != nil || !ok {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, nil
	}
	return fromMS(n), nil
}

// Checkpoint folds the write-ahead log back into the database and truncates
// it. SQLite checkpoints automatically, but an automatic checkpoint cannot
// finish while readers keep using the log, so under steady dashboard traffic
// the log can grow without bound. Running this after large deletes keeps
// it small.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// withTx runs fn in a write transaction.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
