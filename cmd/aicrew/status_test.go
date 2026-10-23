package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

// aicrew status shows a member's hold as its launcher reported it: the
// kind, the amount spent, the limit and the next check.
func TestStatusShowsHold(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	op, err := store.OperatorCaller("op-test")
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.store.CreateTeam(ctx, op, "team", store.NewTeam{Name: "crew"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.store.CreateAgent(ctx, op, "agent", store.NewAgent{Label: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddMember(ctx, op, "member", team.ID, a.ID, store.RoleWorker); err != nil {
		t.Fatal(err)
	}
	// The launcher's report, as aicrewd keeps it (the route's own test is
	// the server's).
	db, err := sql.Open("sqlite", s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	next := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	const layout = "2006-01-02T15:04:05.000000000Z07:00" // the store's
	if _, err := db.Exec(`INSERT INTO member_holds (team_id, agent_id, kind, attempt_id, spent_usd, limit_usd, next_check,
		reset_at, command, identity, since, updated_at)
		VALUES (?, ?, 'daily_budget', '', 26.5, 25, ?, '', '', 'daily_budget/2026-10-23', ?, ?)`,
		team.ID, a.ID, next.Format(layout), next.Add(-time.Hour).Format(layout),
		next.Add(-time.Hour).Format(layout)); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(ctx, []string{"status", "--team", team.ID}, &out, &errb); code != 0 {
		t.Fatalf("status: %d %s", code, errb.String())
	}
	var list []opapi.TeamState
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 || len(list[0].Members) != 1 {
		t.Fatalf("status: %s %v", out.String(), err)
	}
	h := list[0].Members[0].Hold
	if h == nil || h.Kind != "daily_budget" || h.SpentUSD != 26.5 || h.LimitUSD != 25 || !h.NextCheck.Equal(next) {
		t.Fatalf("the hold: %s", out.String())
	}
}
