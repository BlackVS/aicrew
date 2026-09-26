// Package store is aicrew's durable coordination store.
//
// It holds the registry (agents, teams with their project sets, memberships
// with roles) and team sessions with generation fencing. It follows
// docs/CREW-CONTRACT.md:
//
//   - Every mutation takes an explicit Caller. Only an operator caller may
//     change the registry; the zero-value Caller has no authority. The store
//     enforces policy; authentication belongs to the surface that calls it.
//   - Labels, model and client data describe records and never authorize.
//   - A team's project list records intended scope only. Access to a project
//     is decided by aimem grants, never by this list.
//   - A linked aimem identity is set only by a verified proof (identity.go):
//     a verifier redeems the agent's aimem receipt for a challenge the store
//     issued. Failed or unavailable verification changes nothing. One aimem
//     user links to at most one agent. Starting a session requires a link.
//   - Receipts are secrets: they never enter digests, audit or storage.
//   - Invitation codes are secrets too: the store keeps only their digest
//     (invitestore.go), never the code, not even in retry receipts.
//   - A session command names its session and generation. A stale generation
//     is refused, and a replayed result is returned only while the session
//     still has the generation recorded in it.
//   - Each team has one ordered inbox (messages.go). A read always starts at
//     the caller's oldest unacknowledged message, so nothing delivered but
//     unacknowledged is ever skipped, even across a restart.
//   - An attempt offers a task to one worker and holds the worker's one
//     execution capacity (attempts.go). Aimem decides who holds the task:
//     each step records its intent, calls the reservation port with no
//     transaction open, then commits the confirmed outcome. An unknown
//     outcome keeps the capacity and reconciles by receipt.
//   - A running attempt's work (work.go) keeps an append-only result
//     history; an acceptance names one result and one reviewing session,
//     and finalizing as DONE needs the process's delivery evidence
//     (delivery.go), supplied by a trusted internal caller.
//   - Reads that span several statements see one consistent snapshot.
//   - Each mutation commits its state change, audit record and idempotency
//     receipt in one transaction.
//   - An identity proof or redemption retried while the original is still
//     running waits for it and then answers from its receipt (inflight.go).
//     This holds within one Store, so a database file is served by one
//     Store: Open refuses a file another Store has open (lock.go).
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
const schemaVersion = 11

var ErrSchemaTooNew = errors.New("store schema is newer than this build")

// Store is an open aicrew store. It is safe for concurrent use.
type Store struct {
	db   *sql.DB // writes: immediate transactions
	rdb  *sql.DB // reads: deferred, query-only transactions
	lock *storeLock
	now  func() time.Time

	// flights serializes identical commands that call out before their
	// transaction; flightWait bounds how long one waits (inflight.go).
	flights    inflight
	flightWait time.Duration

	// beforeReceipt, when set by tests, runs after the state change and audit
	// record are written and before the receipt, to prove rollback.
	beforeReceipt func(op string) error
	// afterReceiptLookup, when set by tests, runs when a command that calls
	// out before its transaction found no receipt, to force interleavings.
	afterReceiptLookup func(op string)

	// outstandingWork reports whether an agent holds work that must be
	// reconciled before its identity changes: an open attempt or an offer
	// not yet running (attempts.go). Tests may replace it.
	outstandingWork func(ctx context.Context, tx *sql.Tx, agentID string) (bool, error)
}

// Open opens or creates the store at path and applies the schema. It
// refuses with ErrStoreInUse while another Store has the file open (lock.go).
func Open(ctx context.Context, path string) (_ *Store, err error) {
	if path == "" || strings.Contains(path, "?") {
		return nil, fmt.Errorf("%w: store path must be non-empty and contain no '?'", ErrInvalid)
	}
	lock, err := lockStore(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.release() //nolint:errcheck // the open error is the one to report
		}
	}()
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
	s := &Store{
		db:         db,
		lock:       lock,
		now:        func() time.Time { return time.Now().UTC() },
		flightWait: defaultInflightWait,
		outstandingWork: func(ctx context.Context, tx *sql.Tx, agentID string) (bool, error) {
			return openWork(ctx, tx, agentID, "")
		},
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Readers use deferred transactions: in WAL mode a read transaction sees
	// one snapshot from its first statement until it ends, without taking the
	// write lock.
	rdb, err := sql.Open("sqlite", path+"?_txlock=deferred"+
		"&_pragma=busy_timeout(10000)"+
		"&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open store reader: %w", err)
	}
	s.rdb = rdb
	return s, nil
}

// Close closes the store and releases its file lock.
func (s *Store) Close() error {
	return errors.Join(s.rdb.Close(), s.db.Close(), s.lock.release())
}

// snapshot runs fn in one read transaction, so every statement it issues
// sees the same committed state.
func (s *Store) snapshot(ctx context.Context, fn func(q querier) error) error {
	tx, err := s.rdb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin read: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	return fn(tx)
}

func (s *Store) migrate(ctx context.Context) (err error) {
	// A step may rebuild a table that others reference, which SQLite allows
	// only with foreign keys off, and that pragma has no effect inside a
	// transaction. So the migration runs on one connection with foreign
	// keys off, checks every reference before it commits, and turns them
	// back on before the connection returns to the pool.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // a failed migration closes the store
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration: %w", err)
	}
	defer func() {
		var on int
		_, xerr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
		if xerr == nil {
			xerr = conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on)
		}
		if xerr == nil && on != 1 {
			xerr = errors.New("foreign keys are off")
		}
		if xerr != nil && err == nil {
			err = fmt.Errorf("re-enable foreign keys after migration: %w", xerr)
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
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
	// Each step upgrades the schema by one version.
	steps := [][]string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7, schemaV8, schemaV9,
		schemaV10, schemaV11}
	for v := version; v < schemaVersion; v++ {
		for _, stmt := range steps[v] {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply schema v%d: %w", v+1, err)
			}
		}
	}
	record := `UPDATE schema_version SET version = ?`
	if version == 0 {
		record = `INSERT INTO schema_version (version) VALUES (?)`
	}
	if _, err := tx.ExecContext(ctx, record, schemaVersion); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// foreignKeyCheck fails if any row references a row that does not exist.
func foreignKeyCheck(ctx context.Context, q querier) error {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fk int
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			return fmt.Errorf("foreign key check: %w", err)
		}
		return fmt.Errorf("foreign key check: a row of %s references a missing row of %s", table, parent)
	}
	return rows.Err()
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

// schemaV3 adds identity proof: the linked credential's token ID, one agent
// per aimem identity, the token a session is bound to, and challenges.
var schemaV3 = []string{
	`ALTER TABLE agents ADD COLUMN linked_token_id TEXT`,
	`CREATE UNIQUE INDEX agents_one_per_identity ON agents (linked_hub_id, linked_user_id)
		WHERE linked_hub_id IS NOT NULL`,
	`ALTER TABLE sessions ADD COLUMN token_id TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE challenges (
		id            TEXT PRIMARY KEY,
		kind          TEXT NOT NULL CHECK (kind IN ('agent', 'invitation')),
		agent_id      TEXT REFERENCES agents (id),
		invitation_id TEXT,
		hub_id        TEXT NOT NULL,
		state         TEXT NOT NULL CHECK (state IN ('pending', 'consumed', 'superseded')),
		expires_at    TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		updated_at    TEXT NOT NULL,
		CHECK ((kind = 'agent' AND agent_id IS NOT NULL AND invitation_id IS NULL) OR
		       (kind = 'invitation' AND invitation_id IS NOT NULL AND agent_id IS NULL))
	)`,
	`CREATE INDEX challenges_invitation ON challenges (invitation_id) WHERE invitation_id IS NOT NULL`,
}

// schemaV4 adds operator-issued invitations, stored by code digest only.
var schemaV4 = []string{
	`CREATE TABLE invitations (
		id               TEXT PRIMARY KEY,
		code_digest      TEXT NOT NULL UNIQUE,
		purpose          TEXT NOT NULL CHECK (purpose IN ('join', 'link', 'rebind')),
		team_id          TEXT NOT NULL REFERENCES teams (id),
		role             TEXT NOT NULL CHECK (role IN ('coordinator', 'worker', 'independent')),
		hub_id           TEXT NOT NULL,
		agent_id         TEXT NOT NULL DEFAULT '',
		expected_user_id TEXT NOT NULL DEFAULT '',
		label            TEXT NOT NULL DEFAULT '',
		issued_by        TEXT NOT NULL,
		state            TEXT NOT NULL CHECK (state IN ('issued', 'redeemed', 'revoked', 'locked')),
		attempts         INTEGER NOT NULL CHECK (attempts >= 0),
		revision         INTEGER NOT NULL,
		expires_at       TEXT NOT NULL,
		created_at       TEXT NOT NULL,
		updated_at       TEXT NOT NULL
	)`,
}

// schemaV5 adds the team inbox: one ordered message log per team, and one
// row per recipient recording its deliveries and acknowledgement.
var schemaV5 = []string{
	`CREATE TABLE messages (
		id                    TEXT PRIMARY KEY,
		team_id               TEXT NOT NULL REFERENCES teams (id),
		seq                   INTEGER NOT NULL CHECK (seq > 0),
		kind                  TEXT NOT NULL CHECK (kind IN ('message', 'lifecycle')),
		sender_agent_id       TEXT NOT NULL DEFAULT '',
		sender_session_id     TEXT NOT NULL DEFAULT '',
		sender_generation     INTEGER NOT NULL DEFAULT 0,
		sender_model          TEXT NOT NULL DEFAULT '',
		sender_client         TEXT NOT NULL DEFAULT '',
		sender_client_version TEXT NOT NULL DEFAULT '',
		to_agent_id           TEXT NOT NULL DEFAULT '',
		project_hub_id        TEXT NOT NULL DEFAULT '',
		project_id            TEXT NOT NULL DEFAULT '',
		task_hub_id           TEXT NOT NULL DEFAULT '',
		task_project_id       TEXT NOT NULL DEFAULT '',
		task_id               TEXT NOT NULL DEFAULT '',
		text                  TEXT NOT NULL,
		created_at            TEXT NOT NULL,
		UNIQUE (team_id, seq)
	)`,
	`CREATE TABLE message_recipients (
		message_id         TEXT NOT NULL REFERENCES messages (id),
		agent_id           TEXT NOT NULL REFERENCES agents (id),
		deliveries         INTEGER NOT NULL DEFAULT 0 CHECK (deliveries >= 0),
		first_delivered_at TEXT,
		last_delivered_at  TEXT,
		acknowledged_at    TEXT,
		PRIMARY KEY (message_id, agent_id)
	)`,
	`CREATE INDEX message_recipients_pending ON message_recipients (agent_id, acknowledged_at)`,
}

// schemaV11 lets an attempt start from an independent member's claim as
// well as from a coordinator's offer (docs/CREW-CONTRACT.md, "Independent
// claim"). A claim has no coordinator and a claiming state, which the
// attempts table's constraints could not express, so the table is rebuilt
// with every row and index kept: origin says how the attempt began, a
// coordinator is recorded exactly for offers, and the state set gains
// claiming. The migration checks every reference before it commits.
var schemaV11 = []string{
	`CREATE TABLE attempts_v11 (
		id                     TEXT PRIMARY KEY,
		team_id                TEXT NOT NULL REFERENCES teams (id),
		task_hub_id            TEXT NOT NULL,
		task_project_id        TEXT NOT NULL,
		task_id                TEXT NOT NULL,
		worker_agent_id        TEXT NOT NULL REFERENCES agents (id),
		origin                 TEXT NOT NULL CHECK (origin IN ('offer', 'claim')),
		coordinator_agent_id   TEXT REFERENCES agents (id),
		coordinator_session_id TEXT NOT NULL,
		coordinator_generation INTEGER NOT NULL,
		state                  TEXT NOT NULL CHECK (state IN
			('offering', 'offered', 'accepting', 'claiming', 'running', 'releasing', 'reconciling', 'closed')),
		close_reason           TEXT NOT NULL DEFAULT '',
		declined               INTEGER NOT NULL DEFAULT 0 CHECK (declined IN (0, 1)),
		base_commit            TEXT NOT NULL,
		branch                 TEXT NOT NULL,
		process_repository     TEXT NOT NULL,
		process_commit         TEXT NOT NULL,
		process_manifest       TEXT NOT NULL,
		instruction_digest     TEXT NOT NULL,
		offer_expires_at       TEXT NOT NULL,
		task_revision          INTEGER NOT NULL,
		reservation_id         TEXT NOT NULL DEFAULT '',
		fence                  TEXT NOT NULL DEFAULT '',
		last_receipt_id        TEXT NOT NULL DEFAULT '',
		last_refusal           TEXT NOT NULL DEFAULT '',
		pending_op             TEXT NOT NULL DEFAULT '',
		pending_key            TEXT NOT NULL DEFAULT '',
		pending_from           TEXT NOT NULL DEFAULT '',
		intents                INTEGER NOT NULL DEFAULT 0,
		revision               INTEGER NOT NULL,
		created_at             TEXT NOT NULL,
		updated_at             TEXT NOT NULL,
		worker_session_id      TEXT NOT NULL DEFAULT '',
		worker_generation      INTEGER NOT NULL DEFAULT 0,
		worker_session_floor   INTEGER NOT NULL DEFAULT -1,
		phase                  TEXT NOT NULL DEFAULT '' CHECK (phase IN
			('', 'working', 'blocked', 'submitted', 'rework', 'accepted', 'finalized')),
		pending_intent         TEXT NOT NULL DEFAULT '',
		pending_detail         TEXT NOT NULL DEFAULT '',
		pending_evidence       TEXT NOT NULL DEFAULT '',
		pending_message        TEXT NOT NULL DEFAULT '',
		accepted_result        INTEGER NOT NULL DEFAULT 0,
		accepted_by_session    TEXT NOT NULL DEFAULT '',
		accepted_by_generation INTEGER NOT NULL DEFAULT 0,
		finalized_result       INTEGER NOT NULL DEFAULT 0,
		terminal_evidence      TEXT NOT NULL DEFAULT '',
		stop                   TEXT NOT NULL DEFAULT '' CHECK (stop IN ('', 'requested', 'confirmed')),
		stop_by                TEXT NOT NULL DEFAULT '',
		stop_session           TEXT NOT NULL DEFAULT '',
		stop_reason            TEXT NOT NULL DEFAULT '',
		stop_at                TEXT NOT NULL DEFAULT '',
		CHECK ((origin = 'offer') = (coordinator_agent_id IS NOT NULL))
	)`,
	`INSERT INTO attempts_v11 (` + attemptsV10Columns + `, origin)
	 SELECT ` + attemptsV10Columns + `, 'offer' FROM attempts`,
	`DROP TABLE attempts`,
	`ALTER TABLE attempts_v11 RENAME TO attempts`,
	`CREATE UNIQUE INDEX attempts_one_open_per_worker ON attempts (worker_agent_id) WHERE state != 'closed'`,
	`CREATE INDEX attempts_coordinator ON attempts (coordinator_agent_id) WHERE state != 'closed'`,
}

// attemptsV10Columns are the attempts columns before schema v11, all
// carried over as they are.
const attemptsV10Columns = `id, team_id, task_hub_id, task_project_id, task_id, worker_agent_id,
	coordinator_agent_id, coordinator_session_id, coordinator_generation, state, close_reason, declined,
	base_commit, branch, process_repository, process_commit, process_manifest, instruction_digest,
	offer_expires_at, task_revision, reservation_id, fence, last_receipt_id, last_refusal, pending_op,
	pending_key, pending_from, intents, revision, created_at, updated_at, worker_session_id,
	worker_generation, worker_session_floor, phase, pending_intent, pending_detail, pending_evidence,
	pending_message, accepted_result, accepted_by_session, accepted_by_generation, finalized_result,
	terminal_evidence, stop, stop_by, stop_session, stop_reason, stop_at`

// schemaV10 adds the stop of a running attempt (stop.go): its state, who
// requested it, from which session, why and when. It is kept apart from the
// work phase so that settling a work step never clears it.
var schemaV10 = []string{
	`ALTER TABLE attempts ADD COLUMN stop TEXT NOT NULL DEFAULT '' CHECK (stop IN ('', 'requested', 'confirmed'))`,
	`ALTER TABLE attempts ADD COLUMN stop_by TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN stop_session TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN stop_at TEXT NOT NULL DEFAULT ''`,
}

// schemaV9 adds the work lifecycle of a running attempt (work.go): its
// phase, the pending work step, the acceptance of one result by one
// reviewing session, the finalized result and its delivery evidence, and an
// append-only history of submitted results.
var schemaV9 = []string{
	`ALTER TABLE attempts ADD COLUMN phase TEXT NOT NULL DEFAULT '' CHECK (phase IN
		('', 'working', 'blocked', 'submitted', 'rework', 'accepted', 'finalized'))`,
	`UPDATE attempts SET phase = 'working' WHERE state = 'running'`,
	`ALTER TABLE attempts ADD COLUMN pending_intent TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN pending_detail TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN pending_evidence TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN pending_message TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN accepted_result INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE attempts ADD COLUMN accepted_by_session TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN accepted_by_generation INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE attempts ADD COLUMN finalized_result INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE attempts ADD COLUMN terminal_evidence TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE attempt_results (
		attempt_id   TEXT NOT NULL REFERENCES attempts (id),
		seq          INTEGER NOT NULL CHECK (seq > 0),
		result_ref   TEXT NOT NULL,
		request_key  TEXT NOT NULL,
		receipt_id   TEXT NOT NULL,
		submitted_at TEXT NOT NULL,
		PRIMARY KEY (attempt_id, seq)
	)`,
}

// schemaV8 keeps a monotonic history of each member's sessions in a team,
// numbered in the order they started (existing sessions by start time, then
// ID), and records on an offer the worker's highest session number when it
// was issued. Offers from before this version record -1: unknown.
var schemaV8 = []string{
	`ALTER TABLE sessions ADD COLUMN ordinal INTEGER NOT NULL DEFAULT 0`,
	`UPDATE sessions SET ordinal = (SELECT COUNT(*) FROM sessions s2
		WHERE s2.team_id = sessions.team_id AND s2.agent_id = sessions.agent_id
		  AND (s2.created_at < sessions.created_at OR (s2.created_at = sessions.created_at AND s2.id <= sessions.id)))`,
	`CREATE UNIQUE INDEX sessions_member_ordinal ON sessions (team_id, agent_id, ordinal)`,
	`ALTER TABLE attempts ADD COLUMN worker_session_floor INTEGER NOT NULL DEFAULT -1`,
}

// schemaV7 binds an offer to the worker's session context when it was
// issued: the worker's active session and generation, or none.
var schemaV7 = []string{
	`ALTER TABLE attempts ADD COLUMN worker_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN worker_generation INTEGER NOT NULL DEFAULT 0`,
}

// schemaV6 adds execution attempts. An attempt that is not closed holds its
// worker's one execution capacity: the partial unique index allows one open
// attempt per worker agent across all teams. attempt_steps keeps the known
// outcome of each reservation step by its request key, so a retried command
// reports its own step's outcome.
var schemaV6 = []string{
	`CREATE TABLE attempts (
		id                     TEXT PRIMARY KEY,
		team_id                TEXT NOT NULL REFERENCES teams (id),
		task_hub_id            TEXT NOT NULL,
		task_project_id        TEXT NOT NULL,
		task_id                TEXT NOT NULL,
		worker_agent_id        TEXT NOT NULL REFERENCES agents (id),
		coordinator_agent_id   TEXT NOT NULL REFERENCES agents (id),
		coordinator_session_id TEXT NOT NULL,
		coordinator_generation INTEGER NOT NULL,
		state                  TEXT NOT NULL CHECK (state IN
			('offering', 'offered', 'accepting', 'running', 'releasing', 'reconciling', 'closed')),
		close_reason           TEXT NOT NULL DEFAULT '',
		declined               INTEGER NOT NULL DEFAULT 0 CHECK (declined IN (0, 1)),
		base_commit            TEXT NOT NULL,
		branch                 TEXT NOT NULL,
		process_repository     TEXT NOT NULL,
		process_commit         TEXT NOT NULL,
		process_manifest       TEXT NOT NULL,
		instruction_digest     TEXT NOT NULL,
		offer_expires_at       TEXT NOT NULL,
		task_revision          INTEGER NOT NULL,
		reservation_id         TEXT NOT NULL DEFAULT '',
		fence                  TEXT NOT NULL DEFAULT '',
		last_receipt_id        TEXT NOT NULL DEFAULT '',
		last_refusal           TEXT NOT NULL DEFAULT '',
		pending_op             TEXT NOT NULL DEFAULT '',
		pending_key            TEXT NOT NULL DEFAULT '',
		pending_from           TEXT NOT NULL DEFAULT '',
		intents                INTEGER NOT NULL DEFAULT 0,
		revision               INTEGER NOT NULL,
		created_at             TEXT NOT NULL,
		updated_at             TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX attempts_one_open_per_worker ON attempts (worker_agent_id) WHERE state != 'closed'`,
	`CREATE TABLE attempt_steps (
		request_key TEXT PRIMARY KEY,
		attempt_id  TEXT NOT NULL REFERENCES attempts (id),
		operation   TEXT NOT NULL,
		outcome     TEXT NOT NULL CHECK (outcome IN ('committed', 'refused', 'not_committed')),
		refusal     TEXT NOT NULL DEFAULT '',
		receipt_id  TEXT NOT NULL DEFAULT '',
		settled_at  TEXT NOT NULL
	)`,
	`CREATE INDEX attempts_coordinator ON attempts (coordinator_agent_id) WHERE state != 'closed'`,
}

// timeLayout is fixed width so stored timestamps sort correctly as text.
// schemaV2 adds team sessions and the team-wide coordinator generation.
var schemaV2 = []string{
	`ALTER TABLE teams ADD COLUMN coordinator_generation INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE sessions (
		id                     TEXT PRIMARY KEY,
		team_id                TEXT NOT NULL REFERENCES teams (id),
		agent_id               TEXT NOT NULL REFERENCES agents (id),
		role                   TEXT NOT NULL CHECK (role IN ('coordinator', 'worker', 'independent')),
		state                  TEXT NOT NULL CHECK (state IN ('active', 'left', 'stopped', 'ended')),
		generation             INTEGER NOT NULL CHECK (generation >= 1),
		coordinator_generation INTEGER NOT NULL,
		last_seen_at           TEXT NOT NULL,
		created_at             TEXT NOT NULL,
		updated_at             TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX sessions_one_active_per_member ON sessions (team_id, agent_id) WHERE state = 'active'`,
	`CREATE UNIQUE INDEX sessions_one_active_coordinator ON sessions (team_id)
		WHERE state = 'active' AND role = 'coordinator'`,
}

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
