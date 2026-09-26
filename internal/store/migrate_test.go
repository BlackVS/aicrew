package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// populatedStore fills a store, through the store's own commands, with every
// kind of attempt row an offer produces (offered, declined, withdrawn,
// refused at the claim, working, blocked, accepted, reconciling, stop
// requested, stopped and finalized) and with the steps and results that
// reference them.
func populatedStore(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	s, path := openTemp(t)
	e := newExecTeam(t, s)
	worker := func(label string) execTeam {
		t.Helper()
		ex := e
		ex.builder = joinCrew(t, s, e.tm.ID, label, RoleWorker)
		return ex
	}
	running := func(label, taskID string) (execTeam, Attempt) {
		t.Helper()
		ex := worker(label)
		return ex, ex.accept(t, "accept-"+label, ex.offer(t, "offer-"+label, taskID))
	}

	worker("offered").offer(t, "o-offered", "task-offered")
	declined := worker("declined")
	d := declined.offer(t, "o-declined", "task-declined")
	if _, err := s.DeclineOffer(ctx, declined.builder.caller, "decline", d.ID, declined.builder.sess.ID, declined.builder.sess.Generation); err != nil {
		t.Fatal(err)
	}
	withdrawn := worker("withdrawn")
	if _, err := withdrawn.release(t, "r-withdrawn", withdrawn.offer(t, "o-withdrawn", "task-withdrawn")); err != nil {
		t.Fatal(err)
	}
	// A second offer of a task aimem already holds is refused at the claim.
	refused := worker("refused")
	if _, err := s.OfferTask(ctx, e.lead.caller, e.port, "o-refused", refused.offerReq("task-offered")); err == nil {
		t.Fatal("a second offer of a held task succeeded")
	}

	running("working", "task-working")
	ex, a := running("blocked", "task-blocked")
	ex.mustWork(t, "block", a, IntentBlock, "waiting on a decision")
	ex, a = running("accepted", "task-accepted")
	ex.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
	if _, err := ex.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	ex, a = running("reconciling", "task-reconciling")
	e.port.faults[ReservationUpdate] = faultLostReply
	if _, err := ex.work(t, "submit", a, IntentSubmit, "https://example.invalid/pull/2"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("lost reply: %v", err)
	}
	ex, a = running("stopping", "task-stopping")
	if _, err := ex.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	ex, a = running("stopped", "task-stopped")
	if _, err := ex.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.confirmStop(t, "confirm", a); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.releaseStopped(t, "release", a, ReleaseBlocked, "needs a decision"); err != nil {
		t.Fatal(err)
	}
	ex, a = running("finalized", "task-finalized")
	ex.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/3")
	if _, err := ex.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.finalize(t, "final", a, ex.builder, 1, devDelivery("3")); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// downgradeToV10 rebuilds the attempts table in its schema v10 shape (the v6
// table and the v7 to v10 changes to it), keeping every row, and records
// version 10: the store a v10 build left behind.
func downgradeToV10(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path) // foreign keys off: SQLite's default
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	stmts := []string{strings.Replace(schemaV6[0], "CREATE TABLE attempts (", "CREATE TABLE attempts_v10 (", 1)}
	for _, step := range [][]string{schemaV7, schemaV8, schemaV9, schemaV10} {
		for _, stmt := range step {
			if strings.HasPrefix(stmt, "ALTER TABLE attempts ") {
				stmts = append(stmts, strings.Replace(stmt, "ALTER TABLE attempts ", "ALTER TABLE attempts_v10 ", 1))
			}
		}
	}
	stmts = append(stmts,
		`INSERT INTO attempts_v10 (`+attemptsV10Columns+`) SELECT `+attemptsV10Columns+` FROM attempts`,
		`DROP TABLE attempts`,
		`ALTER TABLE attempts_v10 RENAME TO attempts`,
		schemaV6[1], schemaV6[3],
		`UPDATE schema_version SET version = 10`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("downgrade: %q: %v", stmt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'attempts'`).Scan(&ddl); err != nil ||
		strings.Contains(ddl, "origin") || !strings.Contains(ddl, "coordinator_agent_id   TEXT NOT NULL") {
		t.Fatalf("downgraded attempts table = %q, %v", ddl, err)
	}
}

// rowsOf reads every row of a query as text, in order.
func rowsOf(t *testing.T, db *sql.DB, query string) [][]string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = fmt.Sprintf("%v:%s", v.Valid, v.String)
		}
		out = append(out, row)
	}
	return out
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

const (
	attemptRows = `SELECT ` + attemptsV10Columns + ` FROM attempts ORDER BY id`
	stepRows    = `SELECT * FROM attempt_steps ORDER BY request_key`
	resultRows  = `SELECT * FROM attempt_results ORDER BY attempt_id, seq`
)

// Schema v11 rebuilds the attempts table of a populated v10 store: every row
// of every kind keeps every value, becomes an offer, and every reference
// still holds; foreign keys are enforced again afterwards, and the store
// works on, claims included.
func TestMigrationV11KeepsEveryAttempt(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeToV10(t, path)
	raw := rawDB(t, path)
	wantAttempts, wantSteps, wantResults := rowsOf(t, raw, attemptRows), rowsOf(t, raw, stepRows), rowsOf(t, raw, resultRows)
	states := map[string]bool{}
	for _, r := range rowsOf(t, raw, `SELECT state || '/' || phase || '/' || stop || '/' || close_reason || '/' || declined FROM attempts`) {
		states[r[0]] = true
	}
	if len(wantAttempts) != 11 || len(wantSteps) == 0 || len(wantResults) == 0 || len(states) != 11 {
		t.Fatalf("population too thin: %d attempts in %d kinds %v, %d steps, %d results",
			len(wantAttempts), len(states), states, len(wantSteps), len(wantResults))
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v10 store: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 11 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if got := rowsOf(t, s2.db, attemptRows); !reflect.DeepEqual(got, wantAttempts) {
		t.Fatalf("attempts changed by the migration:\n got %v\nwant %v", got, wantAttempts)
	}
	if got := rowsOf(t, s2.db, stepRows); !reflect.DeepEqual(got, wantSteps) {
		t.Fatal("attempt steps changed by the migration")
	}
	if got := rowsOf(t, s2.db, resultRows); !reflect.DeepEqual(got, wantResults) {
		t.Fatal("attempt results changed by the migration")
	}
	if got := rowsOf(t, s2.db, `SELECT DISTINCT origin FROM attempts`); !reflect.DeepEqual(got, [][]string{{"true:offer"}}) {
		t.Fatalf("origins = %v, want only offer", got)
	}
	if err := foreignKeyCheck(ctx, s2.db); err != nil {
		t.Fatal(err)
	}
	var on int
	if err := s2.db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil || on != 1 {
		t.Fatalf("foreign keys after the migration = %d, %v", on, err)
	}
	if _, err := s2.db.Exec(`INSERT INTO attempt_results (attempt_id, seq, result_ref, request_key, receipt_id, submitted_at)
		VALUES ('no-such-attempt', 1, 'r', 'k', 'x', 't')`); err == nil {
		t.Fatal("a result for a missing attempt was accepted after the migration")
	}
	for _, idx := range []string{"attempts_one_open_per_worker", "attempts_coordinator"} {
		if n := len(rowsOf(t, s2.db, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = '`+idx+`'`)); n != 1 {
			t.Fatalf("index %s missing after the migration", idx)
		}
	}
	// The migrated store keeps working: an offer's worker is still busy, and
	// a new independent member can claim.
	var openOffer string
	if err := s2.db.QueryRow(`SELECT worker_agent_id FROM attempts WHERE state = 'offered' AND declined = 0`).Scan(&openOffer); err != nil {
		t.Fatal(err)
	}
	if !busy(t, s2, openOffer) {
		t.Fatal("an open offer's worker is not busy after the migration")
	}
	if _, err := s2.SetTeamProjects(ctx, operator(t), "projects", mustTeamByName(t, s2, "crew").ID,
		mustTeamByName(t, s2, "crew").Revision, []ProjectRef{projectA}); err != nil {
		t.Fatal(err)
	}
	tm := mustTeamByName(t, s2, "crew")
	solo := joinCrew(t, s2, tm.ID, "solo", RoleIndependent)
	ce := claimTeam{execTeam: execTeam{s: s2, tm: tm, port: newFakeReservations(t)}, solo: solo}
	if a, err := ce.claim(t, "c1", solo, "task-new"); err != nil || a.State != AttemptRunning || a.Origin != OriginClaim {
		t.Fatalf("claim after the migration = %+v, %v", a, err)
	}
}

// A v10 store with a dangling reference is not migrated: the migration's
// reference check fails, nothing is committed and the store stays at v10.
func TestMigrationV11RefusesDanglingReferences(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeToV10(t, path)
	raw := rawDB(t, path)
	if _, err := raw.Exec(`INSERT INTO attempt_steps (request_key, attempt_id, operation, outcome, settled_at)
		VALUES ('dangling', 'no-such-attempt', 'claim', 'committed', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "foreign key check") {
		t.Fatalf("open with a dangling reference: %v, want the foreign key check to fail", err)
	}
	var version int
	if err := raw.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("schema version after the refused migration = %d, %v; want 10", version, err)
	}
	var ddl string
	if err := raw.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'attempts'`).Scan(&ddl); err != nil || strings.Contains(ddl, "origin") {
		t.Fatalf("attempts table after the refused migration = %q, %v", ddl, err)
	}
}

func mustTeamByName(t *testing.T, s *Store, name string) Team {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM teams WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tm, err := s.GetTeam(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
