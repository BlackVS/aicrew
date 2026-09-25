// Package store is aicrew's durable coordination store.
//
// This increment holds the registry: agents, teams with their project sets,
// and memberships with roles. It follows docs/CREW-CONTRACT.md:
//
//   - Every mutation takes an explicit Caller. Only an operator caller may
//     change the registry; the zero-value Caller has no authority. The store
//     enforces policy; authentication belongs to the surface that calls it.
//   - Labels, model and client data describe records and never authorize.
//   - A team's project list records intended scope only. Access to a project
//     is decided by aimem grants, never by this list.
//   - Linked aimem identities stay unset until verified proof integration
//     exists; no function in this package sets them.
//   - Each mutation commits its state change, audit record and idempotency
//     receipt in one transaction.
//
// The package is internal and has no network, CLI or MCP surface.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// schemaVersion is the newest schema this code understands. Opening a store
// written by newer code fails rather than guessing.
const schemaVersion = 1

var ErrSchemaTooNew = errors.New("store schema is newer than this build")

// Store is an open aicrew store. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// beforeReceipt, when set by tests, runs after the state change and audit
	// record are written and before the receipt, to prove rollback.
	beforeReceipt func(op string) error
}

// Open opens or creates the store at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" || strings.Contains(path, "?") {
		return nil, fmt.Errorf("%w: store path must be non-empty and contain no '?'", ErrInvalid)
	}
	// Writers take the lock at BEGIN (_txlock=immediate) and wait for it
	// (busy_timeout), so concurrent commands serialize instead of failing on
	// a read-to-write lock upgrade.
	dsn := path + "?_txlock=immediate" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		version = 0
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("%w: store has version %d, this build supports %d", ErrSchemaTooNew, version, schemaVersion)
	}
	if version == schemaVersion {
		return tx.Commit()
	}
	for _, stmt := range schemaV1 {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema v1: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return tx.Commit()
}

var schemaV1 = []string{
	`CREATE TABLE agents (
		id             TEXT PRIMARY KEY,
		label          TEXT NOT NULL,
		model          TEXT NOT NULL,
		client         TEXT NOT NULL,
		client_version TEXT NOT NULL,
		linked_hub_id  TEXT,
		linked_user_id TEXT,
		revision       INTEGER NOT NULL,
		created_at     TEXT NOT NULL,
		updated_at     TEXT NOT NULL,
		CHECK ((linked_hub_id IS NULL) = (linked_user_id IS NULL))
	)`,
	`CREATE INDEX agents_label ON agents (label)`,
	`CREATE TABLE teams (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL,
		revision   INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX teams_name ON teams (name)`,
	`CREATE TABLE team_projects (
		team_id    TEXT NOT NULL REFERENCES teams (id),
		hub_id     TEXT NOT NULL,
		project_id TEXT NOT NULL,
		PRIMARY KEY (team_id, hub_id, project_id)
	)`,
	`CREATE TABLE memberships (
		team_id    TEXT NOT NULL REFERENCES teams (id),
		agent_id   TEXT NOT NULL REFERENCES agents (id),
		role       TEXT NOT NULL CHECK (role IN ('coordinator', 'worker', 'independent')),
		removed    INTEGER NOT NULL DEFAULT 0 CHECK (removed IN (0, 1)),
		revision   INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (team_id, agent_id)
	)`,
	`CREATE TABLE audit (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		at           TEXT NOT NULL,
		caller_kind  TEXT NOT NULL,
		caller_id    TEXT NOT NULL,
		operation    TEXT NOT NULL,
		scope        TEXT NOT NULL,
		key          TEXT NOT NULL,
		input_digest TEXT NOT NULL,
		input        TEXT NOT NULL,
		result       TEXT NOT NULL
	)`,
	`CREATE TABLE receipts (
		caller_kind  TEXT NOT NULL,
		caller_id    TEXT NOT NULL,
		operation    TEXT NOT NULL,
		scope        TEXT NOT NULL,
		key          TEXT NOT NULL,
		input_digest TEXT NOT NULL,
		result       TEXT NOT NULL,
		audit_id     INTEGER NOT NULL REFERENCES audit (id),
		created_at   TEXT NOT NULL,
		PRIMARY KEY (caller_kind, caller_id, operation, scope, key)
	)`,
}

// timeLayout is fixed width so stored timestamps sort correctly as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// newID returns a UUIDv7: time-ordered, with 74 random bits.
func newID(now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	ms := uint64(now.UnixMilli())
	for i := range 6 {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
