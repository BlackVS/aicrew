package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
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
	// An offer of a task aimem holds for someone else is refused at the claim.
	refused := worker("refused")
	e.port.standaloneHold(task("task-held-elsewhere"), "someone-else")
	if _, err := s.OfferTask(ctx, e.lead.caller, e.port, "o-refused", refused.offerReq("task-held-elsewhere")); err == nil {
		t.Fatal("an offer of a task held elsewhere succeeded")
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
		// Tables added after v10 that reference attempts go first.
		`DROP TABLE member_holds`, `DROP TABLE turn_reports`, `DROP TABLE attempt_handoffs`, `DROP TABLE attempt_automation_runs`, `DROP TABLE attempt_pull_facts`, `DROP TABLE attempt_blocks`, `DROP TABLE timers`, `DROP TABLE event_runs`, `DROP TABLE event_deliveries`, `DROP TABLE events`,
		`DROP TABLE scan_finals`, `DROP TABLE coordination_proofs`, `DROP TABLE superseded_updates`,
		`DROP TABLE attempt_pulls`, `ALTER TABLE messages DROP COLUMN forge`,
		`INSERT INTO attempts_v10 (`+attemptsV10Columns+`) SELECT `+attemptsV10Columns+` FROM attempts`,
		`DROP TABLE attempts`,
		`ALTER TABLE attempts_v10 RENAME TO attempts`,
		schemaV6[1], schemaV6[3],
		// Tables and columns added after v10.
		`DROP TABLE session_tokens`, `DROP TABLE session_handles`, `DROP TABLE introspection_credentials`,
		`DROP TABLE pauses`, `ALTER TABLE messages DROP COLUMN operator`,
		`DROP TABLE team_toolchains`, `DROP TABLE board_tasks`, `DROP TABLE board_cursors`, `DROP TABLE board_projects`, `ALTER TABLE messages DROP COLUMN board`,
		`ALTER TABLE attempt_steps DROP COLUMN task_revision`,
		`DROP TABLE escalations`, `DROP TABLE architect_credentials`, `ALTER TABLE messages DROP COLUMN escalation`,
		`ALTER TABLE messages DROP COLUMN offer`, `ALTER TABLE messages DROP COLUMN attempt_id`,
		`ALTER TABLE teams DROP COLUMN hub`, `ALTER TABLE teams DROP COLUMN registration_state`,
		`ALTER TABLE teams DROP COLUMN registration_detail`, `ALTER TABLE teams DROP COLUMN registered_name`,
		`ALTER TABLE teams DROP COLUMN registered_at`,
		`DROP TABLE agent_capabilities`,
		`DROP TABLE team_grants`, `ALTER TABLE teams DROP COLUMN grants_state`,
		`ALTER TABLE teams DROP COLUMN grants_read_at`, teamProjectsV1,
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
	stepRows    = `SELECT request_key, attempt_id, operation, outcome, refusal, receipt_id, settled_at FROM attempt_steps ORDER BY request_key`
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
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
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
	tm := grant(t, s2, mustTeamByName(t, s2, "crew").ID, projectA)
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

// Schema v12 adds the introspection tables to a v11 store and keeps every
// reference; the store then issues and answers as usual.
func TestMigrationV12AddsIntrospectionTables(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropDeliveryColumns(t, raw)
	for _, stmt := range []string{`DROP TABLE IF EXISTS attempt_automation_runs`, `DROP TABLE IF EXISTS attempt_pull_facts`, `DROP TABLE IF EXISTS attempt_blocks`, `DROP TABLE IF EXISTS timers`, `DROP TABLE IF EXISTS event_runs`, `DROP TABLE IF EXISTS event_deliveries`, `DROP TABLE IF EXISTS events`, `DROP TABLE coordination_proofs`, `DROP TABLE session_tokens`, `DROP TABLE session_handles`,
		`DROP TABLE introspection_credentials`, `ALTER TABLE attempts DROP COLUMN process_verified_receipt`,
		`UPDATE schema_version SET version = 11`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v11 store: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := foreignKeyCheck(ctx, s2.db); err != nil {
		t.Fatal(err)
	}
	if _, bearer, err := s2.IssueIntrospectionCredential(ctx, operator(t), "k1", "hub-test"); err != nil || bearer == "" {
		t.Fatalf("issue after the migration: %v", err)
	}
}

// Schema v13 adds the session token table to a populated v12 store and keeps
// every reference; a proof entry then issues a token as usual.
func TestMigrationV13AddsSessionTokens(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropDeliveryColumns(t, raw)
	for _, stmt := range []string{`DROP TABLE IF EXISTS attempt_automation_runs`, `DROP TABLE IF EXISTS attempt_pull_facts`, `DROP TABLE IF EXISTS attempt_blocks`, `DROP TABLE IF EXISTS timers`, `DROP TABLE IF EXISTS event_runs`, `DROP TABLE IF EXISTS event_deliveries`, `DROP TABLE IF EXISTS events`, `DROP TABLE coordination_proofs`, `DROP TABLE session_tokens`,
		`ALTER TABLE introspection_credentials DROP COLUMN operations`,
		`ALTER TABLE attempts DROP COLUMN process_verified_receipt`, `UPDATE schema_version SET version = 12`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v12 store: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := foreignKeyCheck(ctx, s2.db); err != nil {
		t.Fatal(err)
	}
	tm := mustTeamByName(t, s2, "crew")
	a, _ := member(t, s2, tm.ID, "after-migration", RoleWorker)
	_, sec := newProver(t, s2).enter("enter", a, tm.ID)
	mustBind(t, s2, sec.Token)
}

// Schema v16 adds the verified-pin receipt to a populated v15 store and keeps
// every attempt. An attempt from before v16 shows its pin as unverified; an
// offer committed after it records the claim receipt that verified its pin.
func TestMigrationV16AddsVerifiedPinReceipt(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropDeliveryColumns(t, raw)
	for _, stmt := range []string{`DROP TABLE IF EXISTS attempt_automation_runs`, `DROP TABLE IF EXISTS attempt_pull_facts`, `DROP TABLE IF EXISTS attempt_blocks`, `DROP TABLE IF EXISTS timers`, `DROP TABLE IF EXISTS event_runs`, `DROP TABLE IF EXISTS event_deliveries`, `DROP TABLE IF EXISTS events`, `ALTER TABLE attempts DROP COLUMN process_verified_receipt`, `UPDATE schema_version SET version = 15`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v15 store: %v", err)
	}
	defer s2.Close()
	var after, verified int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(process_verified_receipt, '')) FROM attempts`).Scan(&after, &verified); err != nil {
		t.Fatal(err)
	}
	if after != before || verified != 0 {
		t.Fatalf("after v16: %d attempts (was %d), %d verified", after, before, verified)
	}
	e := newExecTeamNamed(t, s2, "after-v16")
	a := e.offer(t, "offer-after", "task-after-v16")
	if a.State != AttemptOffered || a.ProcessVerifiedReceipt == "" || a.ProcessVerifiedReceipt != a.LastReceiptID {
		t.Fatalf("an offer after v16: %+v", a)
	}
}

// dropDeliveryColumns takes a store back to before schema v17 (and so v18),
// so a test that downgrades further can migrate it forward again.
func dropDeliveryColumns(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropSupersededUpdates(t, raw)
	for _, c := range []string{"delivery_result", "delivery_evidence", "delivery_by_agent", "delivery_by_session",
		"delivery_by_generation", "delivery_at"} {
		if _, err := raw.Exec(`ALTER TABLE attempts DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}
}

// Schema v17 adds the confirmed-delivery record to a populated v16 store and
// keeps every attempt, none of them confirmed.
func TestMigrationV17AddsDeliveryConfirmation(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropDeliveryColumns(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 16`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v16 store: %v", err)
	}
	defer s2.Close()
	var after, confirmed int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(delivery_result, 0)) FROM attempts`).Scan(&after, &confirmed); err != nil {
		t.Fatal(err)
	}
	if after != before || confirmed != 0 {
		t.Fatalf("after v17: %d attempts (was %d), %d confirmed", after, before, confirmed)
	}
}

// dropSupersededUpdates takes a store back to before schema v18.
func dropSupersededUpdates(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropRecoveredColumns(t, raw)
	if _, err := raw.Exec(`DROP TABLE superseded_updates`); err != nil {
		t.Fatal(err)
	}
}

// dropRecoveredColumns takes a store back to v18.
func dropRecoveredColumns(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropMessageAttempt(t, raw)
	for _, c := range []string{"recovered_by", "recovered_fence", "recovered_at"} {
		if _, err := raw.Exec(`ALTER TABLE attempts DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`DROP TABLE scan_finals`); err != nil {
		t.Fatal(err)
	}
}

// dropMessageOffer takes a store back to v20.
func dropMessageOffer(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropTeamHub(t, raw)
	if _, err := raw.Exec(`ALTER TABLE messages DROP COLUMN offer`); err != nil {
		t.Fatal(err)
	}
}

// teamProjectsV1 is the team_projects table schema v23 drops.
const teamProjectsV1 = `CREATE TABLE team_projects (
		team_id    TEXT NOT NULL REFERENCES teams (id),
		hub_id     TEXT NOT NULL,
		project_id TEXT NOT NULL,
		PRIMARY KEY (team_id, hub_id, project_id)
	)`

// dropMemberHolds takes a store back to v38.
func dropMemberHolds(t *testing.T, raw *sql.DB) {
	t.Helper()
	if _, err := raw.Exec(`DROP TABLE member_holds`); err != nil {
		t.Fatal(err)
	}
}

// dropTurnReports takes a store back to v37.
func dropTurnReports(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropMemberHolds(t, raw)
	if _, err := raw.Exec(`DROP TABLE turn_reports`); err != nil {
		t.Fatal(err)
	}
}

// dropHandoffs takes a store back to v36.
func dropHandoffs(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropTurnReports(t, raw)
	if _, err := raw.Exec(`DROP TABLE attempt_handoffs`); err != nil {
		t.Fatal(err)
	}
}

// dropAutomations takes a store back to v35.
func dropAutomations(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropHandoffs(t, raw)
	if _, err := raw.Exec(`DROP TABLE attempt_automation_runs`); err != nil {
		t.Fatal(err)
	}
}

// dropActions takes a store back to v34. The messages table keeps its
// v35 shape, a superset of v34's: v35 rebuilds it from the named columns.
func dropActions(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropAutomations(t, raw)
	for _, stmt := range []string{`DROP TABLE attempt_pull_facts`, `ALTER TABLE messages DROP COLUMN action`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropSources takes a store back to v33.
func dropSources(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropActions(t, raw)
	for _, stmt := range []string{`DROP TABLE attempt_blocks`, `DROP TABLE timers`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropEvents takes a store back to v32.
func dropEvents(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropSources(t, raw)
	for _, stmt := range []string{`DROP TABLE event_runs`, `DROP TABLE event_deliveries`, `DROP TABLE events`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropTeamToolchains takes a store back to v31.
func dropTeamToolchains(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropEvents(t, raw)
	if _, err := raw.Exec(`DROP TABLE team_toolchains`); err != nil {
		t.Fatal(err)
	}
}

// dropBoardTasks takes a store back to v30.
func dropBoardTasks(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropTeamToolchains(t, raw)
	if _, err := raw.Exec(`DROP TABLE board_tasks`); err != nil {
		t.Fatal(err)
	}
}

// dropForgeEvents takes a store back to v29.
func dropForgeEvents(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropBoardTasks(t, raw)
	for _, stmt := range []string{`DROP TABLE attempt_pulls`, `ALTER TABLE messages DROP COLUMN forge`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropPauses takes a store back to v28.
func dropPauses(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropForgeEvents(t, raw)
	for _, stmt := range []string{`DROP TABLE pauses`, `ALTER TABLE messages DROP COLUMN operator`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropBoard takes a store back to v27.
func dropBoard(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropPauses(t, raw)
	for _, stmt := range []string{`DROP TABLE board_cursors`, `DROP TABLE board_projects`,
		`ALTER TABLE messages DROP COLUMN board`, `ALTER TABLE attempt_steps DROP COLUMN task_revision`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropEscalations takes a store back to v26.
func dropEscalations(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropBoard(t, raw)
	for _, stmt := range []string{`DROP TABLE escalations`, `DROP TABLE architect_credentials`,
		`ALTER TABLE messages DROP COLUMN escalation`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropPendingRefusal takes a store back to v25.
func dropPendingRefusal(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropEscalations(t, raw)
	if _, err := raw.Exec(`ALTER TABLE attempts DROP COLUMN pending_refusal`); err != nil {
		t.Fatal(err)
	}
}

// dropAttemptRepository takes a store back to v23.
func dropAttemptRepository(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropPendingRefusal(t, raw)
	if _, err := raw.Exec(`DROP TABLE agent_capabilities`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"repository_kind", "repository_url", "repository_access", "default_branch",
		"blocked_reason", "blocked_since"} {
		if _, err := raw.Exec(`ALTER TABLE attempts DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
}

// dropTeamGrants takes a store back to v22.
func dropTeamGrants(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropAttemptRepository(t, raw)
	for _, stmt := range []string{`DROP TABLE team_grants`, `ALTER TABLE teams DROP COLUMN grants_state`,
		`ALTER TABLE teams DROP COLUMN grants_read_at`, teamProjectsV1} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dropTeamHub takes a store back to v21.
func dropTeamHub(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropTeamGrants(t, raw)
	for _, col := range []string{"hub", "registration_state", "registration_detail", "registered_name", "registered_at"} {
		if _, err := raw.Exec(`ALTER TABLE teams DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
}

// Schema v22 adds a team's hub and its registration outcome to a populated
// v21 store: every team is kept, naming no hub and no registration.
func TestMigrationV22AddsTheTeamHub(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM teams`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropTeamHub(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 21`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v21 store: %v", err)
	}
	defer s2.Close()
	var after, named int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(hub, '')) + COUNT(NULLIF(registration_state, '')) FROM teams`).
		Scan(&after, &named); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || named != 0 {
		t.Fatalf("after v22: %d teams (was %d), %d naming a hub or a registration", after, before, named)
	}
}

// Schema v26 adds the pending step's reported refusal to a populated v25
// store's attempts: every attempt is kept, with none reported.
func TestMigrationV26AddsThePendingRefusal(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropPendingRefusal(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 25`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v25 store: %v", err)
	}
	defer s2.Close()
	var after, reported int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(pending_refusal, '')) FROM attempts`).Scan(&after, &reported); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || reported != 0 {
		t.Fatalf("after v26: %d attempts (was %d), %d with a reported refusal", after, before, reported)
	}
}

// Schema v31 adds the tasks' board states to a populated v30 store, and
// has the board feed read again from its start: the stored cursor is
// dropped and every granted project's first sight moves to the migration,
// so the history read again fills the states and announces nothing.
func TestMigrationV31ReadsTheBoardAgain(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	tm := mustTeamByName(t, s, "crew")
	if _, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), tm.ID, TeamGrantsRead{HubID: "hub-a", State: GrantsEnabled,
		At: s.now(), Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "project-a"}}}); err != nil {
		t.Fatal(err)
	}
	old := s.now().Add(-time.Hour)
	if _, err := s.RecordBoardPage(ctx, ReconcilerCaller(), "hub-a", BoardPage{Changes: []BoardChange{}, Cursor: "c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE board_projects SET since = ?`, formatTime(old)); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropBoardTasks(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 30`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v30 store: %v", err)
	}
	defer s2.Close()
	rc := ReconcilerCaller()
	if cur, err := s2.BoardCursor(ctx, rc, "hub-a"); err != nil || cur != "" {
		t.Fatalf("the cursor after v31: %q, %v", cur, err)
	}
	var since string
	if err := s2.db.QueryRow(`SELECT since FROM board_projects WHERE hub_id = 'hub-a' AND project_id = 'project-a'`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	first, err := parseTime(since)
	if err != nil || !first.After(old) {
		t.Fatalf("the project's first sight after v31: %q, %v", since, err)
	}
	ready := BoardChange{ProjectID: "project-a", TaskID: "task-r", Revision: 3, From: "BACKLOG", To: "READY", At: old.Add(time.Minute)}
	if n, err := s2.RecordBoardPage(ctx, rc, "hub-a", BoardPage{Changes: []BoardChange{ready}, Cursor: "c2"}); err != nil || n != 0 {
		t.Fatalf("the history read again: %d announced, %v", n, err)
	}
	var after int
	var state string
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT state FROM board_tasks WHERE task_id = 'task-r'`).Scan(&state); err != nil || after != before || state != "READY" {
		t.Fatalf("after the read: %d messages (was %d), the task's state %q, %v", after, before, state, err)
	}
}

// Schema v32 adds the teams' toolchains to a populated v31 store: every
// team is kept with none selected, and one can be selected after.
// Schema v35 rebuilds messages with the action.needed kind and its detail,
// and adds the pull requests' content facts, on a populated v34 store:
// every message is kept with its kind, sequence and details, and an
// action.needed message can be written after it.
func TestMigrationV35AddsActionNeeded(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	snapshot := func(db *sql.DB) string {
		t.Helper()
		var out string
		if err := db.QueryRow(`SELECT COUNT(*) || '/' || group_concat(id || ':' || kind || ':' || seq || ':' || offer || ':' || board, ',')
			FROM (SELECT * FROM messages ORDER BY id)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot(s.db)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropActions(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 34`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v34 store: %v", err)
	}
	defer s2.Close()
	if after := snapshot(s2.db); after != before || strings.HasPrefix(before, "0/") {
		t.Fatalf("after v35 the messages are %s, were %s", after, before)
	}
	var facts int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM attempt_pull_facts`).Scan(&facts); err != nil || facts != 0 {
		t.Fatalf("pull facts: %d, %v", facts, err)
	}
	tm := mustTeamByName(t, s2, "crew")
	tx, err := s2.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, err := insertMessage(ctx, tx, Message{TeamID: tm.ID, Kind: KindActionNeeded, Text: "action.needed: a test",
		Action: &ActionDetail{Subjects: []ActionSubject{{Kind: "pull", Text: "a test"}}}}, nil, s2.now())
	if err != nil || m.Seq == 0 {
		t.Fatalf("an action.needed message after v35: %+v, %v", m, err)
	}
}

// Schema v34 adds the blocked reports and the timers to a populated v33
// store: an attempt blocked before it has no report recorded, and the team
// state shows none, until its next blocked report.
func TestMigrationV34AddsBlockedReportsAndTimers(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropSources(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 33`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v33 store: %v", err)
	}
	defer s2.Close()
	var blocked, reports, timers int
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM attempts WHERE phase = 'blocked'),
		(SELECT COUNT(*) FROM attempt_blocks), (SELECT COUNT(*) FROM timers)`).Scan(&blocked, &reports, &timers); err != nil {
		t.Fatal(err)
	}
	if blocked == 0 || reports != 0 || timers != 0 {
		t.Fatalf("after v34: %d blocked attempts, %d reports, %d timers", blocked, reports, timers)
	}
}

// Schema v33 adds the event journal, its deliveries and the record of
// handler runs to a populated v32 store: every message is kept, nothing is
// journaled or queued, and a delivery finds nothing to run.
func TestMigrationV33AddsTheEventJournal(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropEvents(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 32`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v32 store: %v", err)
	}
	defer s2.Close()
	var after, journaled, queued, runs int
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages), (SELECT COUNT(*) FROM events),
		(SELECT COUNT(*) FROM event_deliveries), (SELECT COUNT(*) FROM event_runs)`).Scan(&after, &journaled, &queued, &runs); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || journaled != 0 || queued != 0 || runs != 0 {
		t.Fatalf("after v33: %d messages (was %d), %d events, %d deliveries, %d runs", after, before, journaled, queued, runs)
	}
	if got := deliver(t, s2); len(got) != 0 {
		t.Fatalf("runs on a new journal: %+v", got)
	}
}

func TestMigrationV32AddsTeamToolchains(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	tm := mustTeamByName(t, s, "crew")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropTeamToolchains(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 31`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v31 store: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetTeam(ctx, tm.ID)
	if err != nil || got.Toolchains != nil || got.Revision != tm.Revision {
		t.Fatalf("the team after v32: %+v, %v", got, err)
	}
	if got, err = s2.SetTeamToolchain(ctx, operator(t), "k", tm.ID, "project-a", "dev-go"); err != nil ||
		got.Toolchains["project-a"] != "dev-go" {
		t.Fatalf("a selection after v32: %+v, %v", got, err)
	}
}

// Schema v30 adds the attempts' pull requests and the forge event a
// message carries to a populated v29 store: every message is kept, carrying
// none, and no pull request is recorded.
func TestMigrationV30AddsForgeEvents(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropForgeEvents(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 29`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v29 store: %v", err)
	}
	defer s2.Close()
	var after, carrying, pulls int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(forge, '')) FROM messages`).Scan(&after, &carrying); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM attempt_pulls`).Scan(&pulls); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || carrying != 0 || pulls != 0 {
		t.Fatalf("after v30: %d messages (was %d), %d carrying a forge event, %d pull requests", after, before, carrying, pulls)
	}
}

// Schema v29 adds the operator's pauses and the note a nudge's message
// carries to a populated v28 store: every message is kept, carrying none,
// and nothing is paused.
func TestMigrationV29AddsPauses(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropPauses(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 28`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v28 store: %v", err)
	}
	defer s2.Close()
	var after, carrying, pauses int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(operator, '')) FROM messages`).Scan(&after, &carrying); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&pauses); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || carrying != 0 || pauses != 0 {
		t.Fatalf("after v29: %d messages (was %d), %d carrying a note, %d pauses", after, before, carrying, pauses)
	}
}

// Schema v28 adds the board's cursors and projects, and the board change a
// message carries, to a populated v27 store: every message is kept,
// carrying none, and the board starts with no cursor.
func TestMigrationV28AddsTheBoard(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropBoard(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 27`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v27 store: %v", err)
	}
	defer s2.Close()
	var after, carrying, cursors, projects int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(board, '')) FROM messages`).Scan(&after, &carrying); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM board_cursors), (SELECT COUNT(*) FROM board_projects)`).
		Scan(&cursors, &projects); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || carrying != 0 || cursors != 0 || projects != 0 {
		t.Fatalf("after v28: %d messages (was %d), %d carrying a board change, %d cursors, %d projects",
			after, before, carrying, cursors, projects)
	}
}

// Schema v27 adds escalations, architect credentials and the escalation an
// answer's message carries to a populated v26 store, keeping every message.
func TestMigrationV27AddsEscalations(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropEscalations(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 26`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v26 store: %v", err)
	}
	defer s2.Close()
	var after, carrying, escalations, creds int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(escalation, '')) FROM messages`).Scan(&after, &carrying); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM escalations), (SELECT COUNT(*) FROM architect_credentials)`).
		Scan(&escalations, &creds); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || carrying != 0 || escalations != 0 || creds != 0 {
		t.Fatalf("after v27: %d messages (was %d), %d carrying an escalation, %d escalations, %d credentials",
			after, before, carrying, escalations, creds)
	}
}

// Schema v24 adds the repository and the block to a populated v23 store's
// attempts: every attempt is kept, naming no repository and not blocked.
func TestMigrationV24AddsTheAttemptRepository(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropAttemptRepository(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 23`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v23 store: %v", err)
	}
	defer s2.Close()
	var after, named int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(repository_url, '')) + COUNT(NULLIF(blocked_reason, '')) FROM attempts`).
		Scan(&after, &named); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || named != 0 {
		t.Fatalf("after v24: %d attempts (was %d), %d naming a repository or a block", after, before, named)
	}
}

// Schema v23 replaces a populated v22 store's project lists with an empty
// grants snapshot: every team is kept, granted nothing until its hub is
// read, and team_projects is gone with the lists it held.
func TestMigrationV23ReplacesTheProjectSet(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM teams`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropTeamGrants(t, raw)
	if _, err := raw.Exec(`INSERT INTO team_projects (team_id, hub_id, project_id) SELECT id, 'hub-a', 'docs' FROM teams`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE schema_version SET version = 22`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v22 store: %v", err)
	}
	defer s2.Close()
	var after, read, grants, old int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(grants_state, '')) + COUNT(NULLIF(grants_read_at, '')) FROM teams`).
		Scan(&after, &read); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM team_grants`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'team_projects'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || read != 0 || grants != 0 || old != 0 {
		t.Fatalf("after v23: %d teams (was %d), %d read, %d grants, team_projects %d", after, before, read, grants, old)
	}
}

// Schema v21 adds the offer an announcement carries to a populated v20
// store and keeps every message, none of them carrying one.
func TestMigrationV21AddsTheMessagesOffer(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropMessageOffer(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 20`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v20 store: %v", err)
	}
	defer s2.Close()
	var after, offers int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(offer, '')) FROM messages`).Scan(&after, &offers); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || offers != 0 {
		t.Fatalf("after v21: %d messages (was %d), %d carrying an offer", after, before, offers)
	}
}

// dropMessageAttempt takes a store back to v19.
func dropMessageAttempt(t *testing.T, raw *sql.DB) {
	t.Helper()
	dropMessageOffer(t, raw)
	if _, err := raw.Exec(`ALTER TABLE messages DROP COLUMN attempt_id`); err != nil {
		t.Fatal(err)
	}
}

// Schema v20 adds the attempt a lifecycle message names to a populated v19
// store and keeps every message, none of them naming one.
func TestMigrationV20AddsTheMessagesAttempt(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropMessageAttempt(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 19`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v19 store: %v", err)
	}
	defer s2.Close()
	var after, named int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(attempt_id, '')) FROM messages`).Scan(&after, &named); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 || named != 0 {
		t.Fatalf("after v20: %d messages (was %d), %d naming an attempt", after, before, named)
	}
}

// Schema v19 adds the recovered closure's evidence to a populated v18 store
// and keeps every attempt, none of them recovered.
func TestMigrationV19AddsRecoveredClosure(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropRecoveredColumns(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 18`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v18 store: %v", err)
	}
	defer s2.Close()
	var after, recovered int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(NULLIF(recovered_by, '')) FROM attempts`).Scan(&after, &recovered); err != nil {
		t.Fatal(err)
	}
	if after != before || recovered != 0 {
		t.Fatalf("after v19: %d attempts (was %d), %d recovered", after, before, recovered)
	}
}

// Schema v18 adds the superseded update keys to a populated v17 store and
// keeps every attempt.
func TestMigrationV18AddsSupersededUpdates(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropSupersededUpdates(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 17`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v17 store: %v", err)
	}
	defer s2.Close()
	var after, keys int
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM attempts), (SELECT COUNT(*) FROM superseded_updates)`).Scan(&after, &keys); err != nil {
		t.Fatal(err)
	}
	if after != before || keys != 0 {
		t.Fatalf("after v18: %d attempts (was %d), %d superseded keys", after, before, keys)
	}
}

// Schema v37 adds the attempts' handoffs to a populated v36 store: every
// attempt is kept, and none has a handoff.
func TestMigrationV37AddsHandoffs(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil || before == 0 {
		t.Fatalf("the populated store's attempts: %d, %v", before, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropHandoffs(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 36`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v36 store: %v", err)
	}
	defer s2.Close()
	var after, handoffs int
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM attempts), (SELECT COUNT(*) FROM attempt_handoffs)`).Scan(&after, &handoffs); err != nil ||
		after != before || handoffs != 0 {
		t.Fatalf("after v37: %d attempts (were %d), %d handoffs, %v", after, before, handoffs, err)
	}
}

// Schema v38 adds the turns' reports to a populated v37 store: every
// attempt is kept, and no turn is reported.
func TestMigrationV38AddsTurnReports(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil || before == 0 {
		t.Fatalf("the populated store's attempts: %d, %v", before, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropTurnReports(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 37`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v37 store: %v", err)
	}
	defer s2.Close()
	var after, turns int
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM attempts), (SELECT COUNT(*) FROM turn_reports)`).Scan(&after, &turns); err != nil ||
		after != before || turns != 0 {
		t.Fatalf("after v38: %d attempts (were %d), %d turns, %v", after, before, turns, err)
	}
}

// Schema v39 adds the members' holds to a populated v38 store: every
// attempt is kept, and no member is held.
func TestMigrationV39AddsMemberHolds(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&before); err != nil || before == 0 {
		t.Fatalf("the populated store's attempts: %d, %v", before, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropMemberHolds(t, raw)
	if _, err := raw.Exec(`UPDATE schema_version SET version = 38`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v38 store: %v", err)
	}
	defer s2.Close()
	var after, holds int
	if err := s2.db.QueryRow(`SELECT (SELECT COUNT(*) FROM attempts), (SELECT COUNT(*) FROM member_holds)`).Scan(&after, &holds); err != nil ||
		after != before || holds != 0 {
		t.Fatalf("after v39: %d attempts (were %d), %d holds, %v", after, before, holds, err)
	}
}
